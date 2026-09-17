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
- **Gate covers trigger admission only — NOT retry or claim.** `AgentReadiness`
  runs on every trigger admission path (assignment, @mention, comment-trigger,
  chat, autopilot dispatch) but is never called from `server/internal/service/task.go`
  — confirmed zero call sites there. `retryableReasons` (task.go, around line
  5133) includes `ReasonAgentProviderNetwork` and `ReasonRuntimeOffline` —
  exactly the failure shape a held provider produces before this change. So a
  task admitted before a hold was configured, which then fails on a provider
  error, is re-queued under the hold with no readiness consult at all; the
  same is true of already-queued work a daemon picks up via the claim path. A
  hold configured after in-flight work exists does not retroactively stop
  that work from reaching the held provider again. Deferred to a follow-up —
  not fixed in this PR.
