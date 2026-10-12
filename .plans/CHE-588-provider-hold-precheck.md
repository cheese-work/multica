# CHE-588 — Pre-dispatch provider-hold check

## Problem

A standing workspace policy hold on a model provider is, at the dispatch layer,
indistinguishable from that provider being down. Dispatch to an OpenAI-backed
agent under the current hold surfaces as:

    agent_error.provider_server_error
    unexpected status 503 Service Unavailable

Identical to a real gateway outage. Cost: 4 issues, ~7h, 6+ wasted dispatch
cycles, recovery run against a healthy backend.

## Approach

Gate at `service.AgentReadiness` — the documented single choke point for
admission. Its own doc comment says "Touch this function, all of them move
together"; 12 call sites already honour `Blocked()`, and it runs before any
provider network call. No new plumbing, no new call-site audit.

Three net-new pieces:

1. `dispatch.ReasonProviderHold` — leaf admission enum (wire/client axis).
2. `taskfailure.ReasonDispatchBlockedProviderHold` = `dispatch_blocked.provider_hold`
   — a NEW top-level namespace, deliberately NOT under `agent_error.*`, so
   fleet-health reads that group by that prefix do not count a policy refusal
   as an agent/provider fault. This is the same defect `environment_prepare_failed`
   was created to fix (#7913).
3. A model→provider resolver + workspace-settings hold reader.

## Design decisions

- **Provider resolution is registry-derived**, per acceptance criteria — the
  model catalog already declares `Provider: "openai"` for every gpt-* model
  (`pkg/agent/models.go:633-641`). No hand-maintained deny-list.
- **Modifier normalization mirrors `metrics/pricing.go`**: strip exactly ONE
  trailing `[...]` context tag and peel `vendor:`/`vendor/` routing prefixes,
  so neither `claude-sonnet-5[1m]` nor `custom:c00-anthropic:claude-sonnet-5`
  can bypass a hold by shape. Exactly one tag, matching pricing.go and the
  frontend — re-stripping would let a leftover tag satisfy a rule that
  correctly rejected the raw form.
- **Holds live in `workspace.settings` JSONB** — no migration; matches the
  established ad-hoc-key convention (`always_redact_env`, `github_enabled`).
- **Fail CLOSED on malformed hold config.** Existing settings readers fail
  OPEN to the permissive default. A policy gate must not: failing open on
  malformed JSON silently disables the check and reproduces exactly the
  silent-passthrough this issue exists to kill. Documented at the divergence.
- **Unknown/unresolvable provider does NOT block.** The hold names a provider;
  an unrecognised model string is not evidence of a match, and blocking on it
  would refuse Claude work whenever the catalog lags a new model id.

## Steps

1. Add `dispatch.ReasonProviderHold` with a doc comment in the house style.
2. Add `taskfailure.ReasonDispatchBlockedProviderHold`, register in
   `allReasons`, confirm `IsAgentError()` returns FALSE for it.
3. New `service/provider_hold.go`: `ResolveModelProvider(model) (string, bool)`,
   `ProviderHoldsFromSettings([]byte) ([]ProviderHold, error)`,
   `ProviderHoldNotice(agent, provider, hold, substitutes)`.
4. Wire into `AgentReadiness` before the runtime lookup (policy precedes
   availability — a held agent on an online runtime must still refuse).
5. Substitutes: non-held, non-archived agents in the workspace.
6. Tests — must pin `dispatch_blocked.provider_hold` apart from
   `agent_error.provider_server_error` so a refactor cannot collapse them.

## Acceptance criteria → coverage

- [ ] OpenAI-backed dispatch under hold → policy error, not 503, no network call
- [ ] Error names agent + provider + hold text
- [ ] Claude-backed dispatch unaffected
- [ ] Regression pins the two error codes apart
- [ ] `[1m]` / `custom:` cannot bypass
- [ ] Malformed hold config fails closed

## Open questions

- Hold config is settings-only in this PR (no UI/CLI writer). Platform-first
  per filer's stated default; an admin surface is a separate follow-up.

## Known coverage limits

Two gaps a reviewer found, both deliberate for now and both now written down
where the person who needs them — whoever configures a hold, then sees a
dispatch happen anyway — can find them, not just in Go source.

- **Unknown models bypass the hold (fail-open, deliberate).** A model absent
  from the static catalog resolves to unknown (`ResolveModelProvider`'s
  `ok=false`) and is never evaluated against a hold, so it dispatches even
  under an active hold on the provider it would actually resolve to once the
  catalog catches up. This is the same tradeoff the acceptance criteria call
  for: blocking on "I don't recognise this model" would turn ordinary catalog
  lag into an outage for every provider that was never held, not just the
  held one. No fix planned — this is the accepted shape of the feature, not a
  bug.
- **Case sensitivity — resolved.** Earlier drafts of this file, and the code
  before this fix, matched `ProviderHold.Provider` against the resolved
  provider case-sensitively. Catalog values are always lowercase, but the
  hold side is human-authored JSON in workspace settings, and this
  workspace's own announcement writes "OpenAI" — so a hold typed the natural
  way silently matched nothing. `findProviderHold` now compares with
  `strings.EqualFold`; no case-sensitivity limit remains.
- **Gate covers trigger admission only — NOT retry or claim — resolved in
  CHE-607.** `AgentReadiness` runs on every trigger admission path
  (assignment, @mention, comment-trigger, chat, autopilot dispatch) but was
  never called from `server/internal/service/task.go`. CHE-607 closed this
  without routing retry/claim through `AgentReadiness` itself (that would
  re-run the runtime lookup both paths have already done) — instead
  `providerHoldBlocksAgent` (agent_ready.go) exposes just the provider-hold
  half, called from four points, with two DIFFERENT refusal shapes chosen
  deliberately per independent review (an initial claim-path draft cancelled
  the claimed task and was rejected — see below):
  - `FailTask`'s in-transaction retry and `MaybeRetryFailedTask` (both share
    `refuseRetryForProviderHold`) CANCEL the just-minted retry child in the
    same transaction that created it, with
    `failure_reason = dispatch_blocked.provider_hold`, and post
    `ProviderHoldNotice` as a visible issue comment after commit. Cancelling
    is correct here: the child never existed before this call, so nothing is
    lost.
  - `claimTask` (`agentBlockedByProviderHold`) instead SKIPS the claim
    entirely, checked BEFORE `ClaimAgentTask` runs — the task stays `queued`,
    completely untouched. A task already sitting `queued` predates the check
    by definition and is real wanted work; cancelling it (the initial design)
    was a one-way door with no re-queue on hold-lift and undelivered
    trigger/coalesced comments, which review correctly called a regression
    worse than the bug being fixed. `queued_expired`'s ordinary TTL sweep is
    the eventual backstop if a hold never lifts.
  - `RetrySourceContextQuickCreate` (the manual quick-create retry button)
    refuses before any row is created, via `ErrSourceContextRetryProviderHeld`.
  - `RerunIssue` (the general manual "rerun" button) — resolved in CHE-675.
    Gated at the one choke point common to every rerun shape (task_id rerun,
    assignee rerun, squad-leader rerun): right after the target agent is
    resolved and before any prior task is cancelled, alongside the existing
    canInvoke re-validation. Refuses via `ErrRerunProviderHeld`, mapped to the
    same `dispatch_blocked.provider_hold` HTTP response as the quick-create
    button.

  No known gaps remain in this list.
