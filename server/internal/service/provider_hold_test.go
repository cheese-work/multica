package service

import (
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

// --- ResolveModelProvider -------------------------------------------------

// TestResolveModelProviderRealModels pins every real model id this workspace
// dispatches today to its correct provider, per the CHE-588 acceptance
// criteria's explicit list. A resolver that gets any of these wrong either
// lets a held OpenAI agent slip through (false negative) or blocks a healthy
// Claude agent (false positive) — both are the outage this issue exists to
// prevent, just aimed at the wrong layer.
func TestResolveModelProviderRealModels(t *testing.T) {
	cases := []struct {
		model        string
		wantProvider string
	}{
		{"claude-opus-5", "anthropic"},
		{"claude-sonnet-5", "anthropic"},
		// [1m] is the context-window tag Claude Code appends; stripping
		// exactly once must still land on the same SKU.
		{"claude-sonnet-5[1m]", "anthropic"},
		{"gpt-5.6-sol", "openai"},
		{"gpt-5.6-terra", "openai"},
		{"gpt-5.6-luna", "openai"},
		{"gpt-6-astra", "openai"},
		// Routing-prefixed id: a Hermes custom-provider wrapper naming its own
		// nested provider tag ahead of the real model id. Peeling must be
		// iterative to reach claude-sonnet-5 in one call.
		{"custom:c00-anthropic:claude-sonnet-5", "anthropic"},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			got, ok := ResolveModelProvider(tc.model)
			if !ok {
				t.Fatalf("ResolveModelProvider(%q): ok=false, want resolved to %q", tc.model, tc.wantProvider)
			}
			if got != tc.wantProvider {
				t.Errorf("ResolveModelProvider(%q) = %q, want %q", tc.model, got, tc.wantProvider)
			}
		})
	}
}

// TestModelProviderRegistryIsCatalogDerivedNotHandMaintained is the
// CHE-588 acceptance-criteria regression: "registry-derived, not
// hand-maintained". It fails in exactly the two ways someone could silently
// regress this behind ResolveModelProvider's unchanged signature:
//
//  1. Reintroducing a hand-copied literal map (e.g. reverting
//     modelProviderRegistry to a `var ... = map[string]string{...}`) — this
//     test's expected values are read live from agent.StaticModelProviders(),
//     not hardcoded here, so a hand-maintained table that quietly drifts from
//     the catalog (a model renamed or dropped upstream, a typo in the mirror)
//     fails this test the same way a totally reverted implementation would.
//  2. The accessor itself silently losing coverage of a real workspace model
//     — each case below is a model id CHE-588's other test
//     (TestResolveModelProviderRealModels) also pins directly, so if this
//     test and that one ever disagree about the same id, something upstream
//     (the catalog, the accessor, or the registry wiring) broke the
//     derivation, not just this test's fixture.
func TestModelProviderRegistryIsCatalogDerivedNotHandMaintained(t *testing.T) {
	derived := agent.StaticModelProviders()
	if len(derived) == 0 {
		t.Fatal("agent.StaticModelProviders() returned no entries; ResolveModelProvider would resolve nothing")
	}

	for _, model := range []string{"claude-opus-5", "claude-sonnet-5", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-6-astra"} {
		wantProvider, wantOK := derived[model]
		if !wantOK {
			t.Fatalf("agent.StaticModelProviders() missing %q; a real workspace model must appear in the static catalogs", model)
		}
		gotProvider, gotOK := ResolveModelProvider(model)
		if !gotOK || gotProvider != wantProvider {
			t.Errorf("ResolveModelProvider(%q) = (%q, %v), want (%q, true) sourced from agent.StaticModelProviders()", model, gotProvider, gotOK, wantProvider)
		}
	}

	// Routing-prefixed id: normalizeModelID must peel the prefix down to a
	// key that exists in the catalog-derived map, exercising the join
	// between this package's own normalization and the imported accessor.
	if got, ok := ResolveModelProvider("custom:c00-anthropic:claude-sonnet-5"); !ok || got != "anthropic" {
		t.Errorf("ResolveModelProvider(%q) = (%q, %v), want (anthropic, true)", "custom:c00-anthropic:claude-sonnet-5", got, ok)
	}
}

// TestResolveModelProviderUnknownDoesNotResolve is the other half of the same
// contract: a model string the registry has never heard of must come back
// unresolved, never guessed at. AgentReadiness relies on ok=false meaning
// "cannot be evaluated" so a catalog that lags one release behind does not
// start blocking Claude dispatches it simply doesn't recognise yet.
func TestResolveModelProviderUnknownDoesNotResolve(t *testing.T) {
	for _, model := range []string{
		"",
		"totally-made-up-model-xyz",
		"gpt-99-nonexistent",
		"   ",
	} {
		if _, ok := ResolveModelProvider(model); ok {
			t.Errorf("ResolveModelProvider(%q): ok=true, want unresolved", model)
		}
	}
}

// TestResolveModelProviderContextTagStrippedOnce pins the single-strip
// discipline mirrored from metrics/pricing.go's PriceForModelAlias: a second,
// nested tag must NOT be peeled, because that would let a leftover tag
// satisfy a rule that correctly rejected the raw (doubly-tagged) form.
func TestResolveModelProviderContextTagStrippedOnce(t *testing.T) {
	// One tag resolves.
	if got, ok := ResolveModelProvider("claude-opus-5[1m]"); !ok || got != "anthropic" {
		t.Fatalf("single-tag id: got (%q, %v), want (anthropic, true)", got, ok)
	}
	// Two tags must NOT resolve — re-stripping would recover the id a single
	// strip already, correctly, failed to match.
	if _, ok := ResolveModelProvider("claude-opus-5[1m][2m]"); ok {
		t.Error("doubly-tagged id resolved; context tag stripping must be exactly one pass")
	}
}

// --- ProviderHoldsFromSettings: fail-closed ------------------------------

// TestProviderHoldsFromSettingsFailsClosed is the fail-closed regression this
// issue calls out explicitly: unlike workspaceAlwaysRedactSecrets and
// autoLinkPRsEnabledForWorkspace (both fail OPEN to their permissive default
// on malformed JSON), a malformed provider-hold config must surface an error,
// not silently behave as "no holds configured" — the latter is the exact
// silent-passthrough defect CHE-588 exists to eliminate.
func TestProviderHoldsFromSettingsFailsClosed(t *testing.T) {
	for _, malformed := range []string{
		`not json`,
		`{"provider_holds": `,
		`{"provider_holds": "not an array"}`,
	} {
		if _, err := ProviderHoldsFromSettings([]byte(malformed)); err == nil {
			t.Errorf("ProviderHoldsFromSettings(%q): err=nil, want a parse error (fail closed)", malformed)
		}
	}
}

// TestProviderHoldsFromSettingsEmptyIsNotMalformed covers the overwhelmingly
// common case: a workspace that has never configured a hold must not error —
// only genuinely malformed JSON is fail-closed territory.
func TestProviderHoldsFromSettingsEmptyIsNotMalformed(t *testing.T) {
	for _, empty := range [][]byte{nil, {}} {
		holds, err := ProviderHoldsFromSettings(empty)
		if err != nil {
			t.Errorf("ProviderHoldsFromSettings(%q): err=%v, want nil", empty, err)
		}
		if len(holds) != 0 {
			t.Errorf("ProviderHoldsFromSettings(%q): got %v holds, want none", empty, holds)
		}
	}
}

func TestProviderHoldsFromSettingsParsesConfiguredHold(t *testing.T) {
	settings := []byte(`{"provider_holds":[{"provider":"openai","text":"Stop all routing to OpenAI based agents. This constraint has no set end date."}]}`)
	holds, err := ProviderHoldsFromSettings(settings)
	if err != nil {
		t.Fatalf("ProviderHoldsFromSettings: unexpected error: %v", err)
	}
	if len(holds) != 1 || holds[0].Provider != "openai" {
		t.Fatalf("got %+v, want one openai hold", holds)
	}
	if !strings.Contains(holds[0].Text, "OpenAI based agents") {
		t.Errorf("hold text lost: %q", holds[0].Text)
	}
}

// --- The pinning test: dispatch_blocked.provider_hold vs agent_error.* ---

// TestProviderHoldReasonIsNotAnAgentError is the regression this issue calls
// out by name: dispatch_blocked.provider_hold must never collapse into
// agent_error.provider_server_error. Before this reason existed, a held
// OpenAI agent's dispatch surfaced as exactly that agent_error code with a
// bare 503 — indistinguishable from a real provider outage, and the cost was
// 4 issues, ~7h, and a recovery run pointed at a healthy backend. Any future
// refactor that merges "the provider didn't answer" reasons together must
// break this test before it can ship.
func TestProviderHoldReasonIsNotAnAgentError(t *testing.T) {
	if got := taskfailure.ReasonDispatchBlockedProviderHold; got.IsAgentError() {
		t.Errorf("taskfailure.ReasonDispatchBlockedProviderHold.IsAgentError() = true, want false: a policy refusal must not count as an agent/provider fault")
	}
	if !taskfailure.ReasonAgentProviderServerError.IsAgentError() {
		t.Fatal("sanity check failed: ReasonAgentProviderServerError.IsAgentError() = false, want true")
	}
	if taskfailure.ReasonDispatchBlockedProviderHold == taskfailure.ReasonAgentProviderServerError {
		t.Fatal("dispatch_blocked.provider_hold must not equal agent_error.provider_server_error")
	}
	if taskfailure.ReasonDispatchBlockedProviderHold.String() == taskfailure.ReasonAgentProviderServerError.String() {
		t.Fatal("wire forms must differ: a client reading failure_reason has to be able to tell these apart")
	}
}

// TestReasonDispatchBlockedProviderHoldRegistered pins that the new reason is
// discoverable through AllReasons() — the Prometheus pre-warm path — so a
// dashboard panel grouping by failure_reason shows a zero series for it from
// process start instead of only appearing after the first real hold refusal.
func TestReasonDispatchBlockedProviderHoldRegistered(t *testing.T) {
	found := false
	for _, r := range taskfailure.AllReasons() {
		if r == taskfailure.ReasonDispatchBlockedProviderHold {
			found = true
			break
		}
	}
	if !found {
		t.Error("taskfailure.ReasonDispatchBlockedProviderHold is missing from AllReasons()")
	}
}

// --- ProviderHoldNotice ---------------------------------------------------

func TestProviderHoldNoticeContainsAgentProviderHoldAndSubstitutes(t *testing.T) {
	hold := ProviderHold{Provider: "openai", Text: "Stop all routing to OpenAI based agents. This constraint has no set end date."}
	notice := ProviderHoldNotice("Nova", "openai", hold, []string{"Kit", "Mika"})
	for _, want := range []string{"Nova", "openai", "Stop all routing to OpenAI based agents", "Kit", "Mika"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice missing %q:\n%s", want, notice)
		}
	}
}

func TestProviderHoldNoticeOmitsSubstitutesWhenNone(t *testing.T) {
	hold := ProviderHold{Provider: "openai", Text: "hold text"}
	notice := ProviderHoldNotice("Nova", "openai", hold, nil)
	if strings.Contains(notice, "not affected") {
		t.Errorf("notice should not claim substitutes exist when none were found:\n%s", notice)
	}
	if !strings.Contains(notice, "Nova") || !strings.Contains(notice, "hold text") {
		t.Errorf("notice must still name the agent and the hold text with no substitutes:\n%s", notice)
	}
}

func TestProviderHoldNoticeDefaultsAgentName(t *testing.T) {
	notice := ProviderHoldNotice("", "openai", ProviderHold{Text: "x"}, nil)
	if !strings.Contains(notice, "The assigned agent") {
		t.Errorf("empty agent name must fall back to a generic label:\n%s", notice)
	}
}
