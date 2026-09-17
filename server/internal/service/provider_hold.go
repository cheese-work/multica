package service

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/multica-ai/multica/server/pkg/agent"
)

// modelProviderRegistry maps a canonical (already-normalized) model id to the
// provider that serves it. It is derived, not hand-maintained: it is built
// once from agent.StaticModelProviders(), which reads the id/Provider pairs
// straight off pkg/agent's own static model catalogs (claudeStaticModels,
// codexStaticModels, cursorStaticModels, copilotStaticModels,
// grokStaticModels, codebuddyStaticModels). This package DOES import
// pkg/agent — there is no cycle: pkg/agent imports nothing from internal/
// (verified via `go list -deps ./pkg/agent`), so the dependency only runs one
// way, service -> agent. That means a model added to the catalog is covered
// here automatically the next time this map is built, with nothing to keep
// in sync by hand. It is NOT a deny-list — nothing here refuses a model; a
// lookup miss simply resolves to "unknown", which ResolveModelProvider treats
// as "do not block" (see its doc comment). A model that exists only via
// dynamic discovery (never appears in any static catalog) also resolves to
// "unknown" for the same reason — see agent.StaticModelProviders' doc comment
// for that scope limit.
var modelProviderRegistry = sync.OnceValue(func() map[string]string {
	return agent.StaticModelProviders()
})

// contextTagRe matches a trailing context-window variant tag such as the
// `[1m]` Claude Code appends to the model id — the exact same shape and the
// same anchoring as metrics/pricing.go's contextTagRe (`\[[^\]]+\]$`, mirrored
// from packages/views/runtimes/utils.ts's stripContextTag), kept as a second
// copy rather than exported from pricing.go because that package is metrics-
// specific and pulling a service dependency into it would run the wrong way
// against the existing layering.
var contextTagRe = regexp.MustCompile(`\[[^\]]+\]$`)

// routingPrefixRe recognises one leading routing-layer segment ahead of a `/`
// or `:` separator — `vendor/model` (opencode-style) or `provider:model`
// (Hermes custom providers). Matches packages/views/runtimes/utils.ts's
// stripProvider test (`^[a-z][a-z0-9_-]*$` against the segment before the
// separator).
var routingPrefixRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]*[/:]`)

// normalizeModelID canonicalizes a model string the same two ways
// metrics/pricing.go's PriceForModelAlias and the frontend's
// canonicalCandidates do, so a hold on "openai" cannot be bypassed by a
// harness-appended context tag or a routing prefix changing the string's
// shape without changing what it resolves to:
//
//  1. Peel routing prefixes iteratively — `custom:c00-anthropic:claude-sonnet-5`
//     is a provider-prefixed id whose model segment is itself
//     provider-prefixed, so one pass is not enough. This mirrors
//     stripProvider in packages/views/runtimes/utils.ts, which loops for the
//     same reason.
//  2. Strip EXACTLY ONE trailing `[...]` context tag — never re-strip the
//     result. This is the opposite discipline from step 1, on purpose: the
//     context tag is something the CALLING HARNESS appends once
//     (`claude-sonnet-5[1m]`), never something nested, so a second tag
//     surviving after the first strip means the id is not a shape this
//     resolver recognises, and retrying would let a leftover tag satisfy a
//     rule that correctly rejected the raw form (see pricing.go's
//     PriceForModelAlias comment — same reasoning, same bug class).
func normalizeModelID(model string) string {
	out := strings.ToLower(strings.TrimSpace(model))
	for {
		loc := routingPrefixRe.FindStringIndex(out)
		if loc == nil {
			break
		}
		out = out[loc[1]:]
	}
	if stripped := contextTagRe.ReplaceAllString(out, ""); stripped != out {
		return stripped
	}
	return out
}

// ResolveModelProvider maps a model id, as stored on db.Agent.Model, to the
// provider that serves it — registry-derived (modelProviderRegistry, built
// from agent.StaticModelProviders), not a hand-maintained deny-list, per
// CHE-588's acceptance criteria.
//
// ok is false for any model this resolver does not recognise. That is
// deliberately NOT evidence the model is provider-less or safe to run under a
// hold — see the caller-side rule in AgentReadiness's provider-hold check: an
// unresolved model must never be treated as "blocked", only ever as "cannot
// be evaluated". The catalog in pkg/agent/models.go grows faster than any
// hand-copied mirror of it can track, and a resolver that blocked on "I don't
// recognise this" would start refusing Claude dispatches the day this table
// merely fell one release behind — turning a provider-hold feature into an
// availability outage for a provider that was never held.
func ResolveModelProvider(model string) (provider string, ok bool) {
	normalized := normalizeModelID(model)
	if normalized == "" {
		return "", false
	}
	provider, ok = modelProviderRegistry()[normalized]
	return provider, ok
}

// ProviderHold is one workspace-level policy refusal on a model provider —
// e.g. "stop routing to OpenAI-based agents" — read from
// workspace.settings.provider_holds.
type ProviderHold struct {
	// Provider is matched against ResolveModelProvider's return value,
	// case-sensitively — both sides are written by this codebase (the
	// catalog's Provider field and whatever wrote the hold), so no normalizer
	// is warranted here the way model ids need one.
	Provider string `json:"provider"`
	// Text is the human-authored policy statement, surfaced verbatim in the
	// notice so the person reading a refusal sees the actual decision, not a
	// paraphrase that can drift from what was actually decided.
	Text string `json:"text"`
}

// providerHoldSettings is the workspace.settings JSONB shape this package
// owns: {"provider_holds": [{"provider": "...", "text": "..."}]}. Kept as an
// unexported decode target, matching the ad-hoc-anonymous-struct convention
// every other settings reader in this codebase uses
// (workspaceAlwaysRedactSecrets, autoLinkPRsEnabledForWorkspace) — settings is
// a shared free-form JSONB column, and giving every feature its own top-level
// key with its own private decode struct is how unrelated features avoid
// fighting over the document's shape.
type providerHoldSettings struct {
	ProviderHolds []ProviderHold `json:"provider_holds"`
}

// ProviderHoldsFromSettings reads the configured provider holds out of a
// workspace's settings JSONB.
//
// This fails CLOSED on malformed input — an explicit divergence from
// workspaceAlwaysRedactSecrets and autoLinkPRsEnabledForWorkspace, which both
// fail OPEN to their permissive default on a json.Unmarshal error. Those two
// are safety-neutral: a feature flag defaulting to its historical value on a
// parse failure just means the feature stays whatever it always was. A policy
// gate is not safety-neutral the same way — failing open here means malformed
// settings JSON (a bad manual edit, a partial write, a future migration that
// half-applies) silently disables the provider-hold check with no signal
// anywhere that it happened, which is the exact silent-passthrough defect
// CHE-588 exists to eliminate: a hold that stops applying without an error is
// indistinguishable, at the call site, from a hold that never existed. The
// caller (AgentReadiness) must therefore treat a non-nil error as "refuse
// admission for this agent", not "proceed as if no hold applies".
//
// Empty/absent settings is not malformed — it is the overwhelmingly common
// case of "this workspace has never configured a hold" — and returns an empty
// slice with a nil error.
func ProviderHoldsFromSettings(settings []byte) ([]ProviderHold, error) {
	if len(settings) == 0 {
		return nil, nil
	}
	var parsed providerHoldSettings
	if err := json.Unmarshal(settings, &parsed); err != nil {
		return nil, fmt.Errorf("parse workspace settings for provider holds: %w", err)
	}
	return parsed.ProviderHolds, nil
}

// findProviderHold returns the first configured hold matching provider, if
// any. Linear scan: holds are a handful of manually-authored policy entries
// per workspace, not a set worth indexing.
func findProviderHold(holds []ProviderHold, provider string) (ProviderHold, bool) {
	for _, h := range holds {
		if h.Provider == provider {
			return h, true
		}
	}
	return ProviderHold{}, false
}

// ProviderHoldNotice is the durable explanation left on an issue when a
// trigger is refused because the target agent resolves to a provider under a
// workspace policy hold. Mirrors RuntimeUnusableNotice's placement and intent
// (agent_ready.go) — one text, built next to the verdict, read by every
// surface that has to explain a BLOCKED verdict to a human.
//
// Names the agent, the provider the hold matched on, and the hold's own text
// verbatim (never paraphrased — the exact policy wording is what someone
// disputing the refusal needs to see), then lists non-held substitutes when
// any are available so the reader has an actionable next step instead of only
// a reason the trigger failed.
func ProviderHoldNotice(agentName, provider string, hold ProviderHold, substitutes []string) string {
	name := agentName
	if name == "" {
		name = "The assigned agent"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s could not start: its model (%s) is on a workspace policy hold, so this trigger was not queued.\n\n", name, provider)
	fmt.Fprintf(&b, "Policy: %s\n", hold.Text)
	if len(substitutes) > 0 {
		fmt.Fprintf(&b, "\nAgents not affected by this hold: %s.", strings.Join(substitutes, ", "))
	}
	return b.String()
}
