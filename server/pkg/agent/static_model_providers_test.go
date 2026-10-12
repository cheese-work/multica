package agent

import "testing"

// TestStaticModelProvidersNonEmptyAndSelfConsistent pins the two invariants
// internal/service's ResolveModelProvider depends on without importing this
// package's test internals directly:
//
//  1. Non-empty — an accessor that silently returned nothing would make
//     every model "unknown" to the provider-hold check, which fails OPEN
//     (unresolved models are never blocked, per ResolveModelProvider's doc
//     comment) — so this specific regression would not panic or error
//     anywhere, it would just quietly stop enforcing any hold at all.
//  2. Self-consistent — no model id is claimed by two different providers
//     across the static catalogs today. StaticModelProviders already drops
//     genuinely conflicting ids rather than guessing (see its doc comment),
//     but a real conflict appearing here signals two catalogs disagreeing
//     about who serves a model, which is worth surfacing loudly rather than
//     silently resolving via exclusion.
func TestStaticModelProvidersNonEmptyAndSelfConsistent(t *testing.T) {
	t.Parallel()

	got := StaticModelProviders()
	if len(got) == 0 {
		t.Fatal("StaticModelProviders() returned no entries")
	}

	// Recompute the raw (pre-conflict-resolution) id->providers sets
	// directly from the same source catalogs the accessor reads, so this
	// test catches a real cross-catalog disagreement even though
	// StaticModelProviders() itself would just quietly drop it.
	sources := [][]Model{
		claudeStaticModels(),
		codexStaticModels(),
		cursorStaticModels(),
		copilotStaticModels(),
		grokStaticModels(),
		codebuddyStaticModels(),
	}
	providersByID := map[string]map[string]bool{}
	for _, models := range sources {
		for _, m := range models {
			if m.Provider == "" {
				continue
			}
			if providersByID[m.ID] == nil {
				providersByID[m.ID] = map[string]bool{}
			}
			providersByID[m.ID][m.Provider] = true
		}
	}
	for id, providers := range providersByID {
		if len(providers) > 1 {
			t.Errorf("model id %q claimed by %d different providers across static catalogs: %v", id, len(providers), providers)
		}
	}

	// Spot-check a couple of real, well-known ids resolve as expected —
	// guards against an accessor that is "non-empty" only because it
	// returned garbage.
	for _, tc := range []struct {
		id           string
		wantProvider string
	}{
		{"claude-opus-5", "anthropic"},
		{"gpt-6-astra", "openai"},
	} {
		if provider, ok := got[tc.id]; !ok || provider != tc.wantProvider {
			t.Errorf("StaticModelProviders()[%q] = (%q, %v), want (%q, true)", tc.id, provider, ok, tc.wantProvider)
		}
	}
}
