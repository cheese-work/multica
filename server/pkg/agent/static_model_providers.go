package agent

import "strings"

// StaticModelProviders returns a model id -> provider lookup built directly
// from this package's static fallback catalogs (claudeStaticModels,
// codexStaticModels, cursorStaticModels, copilotStaticModels,
// grokStaticModels, codebuddyStaticModels). It exists for a consumer that
// needs a cheap id->provider answer — e.g. internal/service's provider-hold
// check — without pulling in the discovery machinery (ACP handshakes,
// subprocess invocation, CLI version probing) that live model discovery
// requires. Nothing here talks to a CLI or the network; it only reads the
// same []Model literals the offline dropdown fallback uses.
//
// Coverage is STATIC-catalog only. Models a runtime only ever advertises
// through dynamic discovery (e.g. Cursor's real catalog, Copilot's live
// account-scoped list) are absent here — this is a deliberate scope limit,
// not a bug: dynamic catalogs are account- and version-dependent, so there is
// no fixed table to derive from. A caller that needs "unknown" to mean
// "definitely provider-less" rather than "not in the static catalogs" is
// using the wrong function.
//
// Keys are the catalog's Model.ID verbatim, LOWERCASED — the static catalogs
// already declare ids in lowercase, but this function lowercases explicitly
// anyway so the contract holds even if a future entry doesn't, and so it
// matches a consumer that lowercases its own lookup key (e.g.
// internal/service's normalizeModelID) without that consumer needing to know
// this map's casing convention. This function does NOT do any other
// normalization (routing-prefix stripping, context-tag stripping, alias
// folding) — that belongs to the consumer, which already owns that logic for
// its own reasons.
//
// If the same id appears in two static catalogs with two DIFFERENT
// providers, that id is dropped entirely rather than picking one arbitrarily
// — a silent wrong-provider answer is worse than a silent "unknown" for a
// caller whose whole job is deciding whether to block a dispatch. As of this
// writing no such conflict exists across the catalogs listed above (verified
// by TestStaticModelProvidersHasNoConflicts); if one is ever introduced, both
// providers are excluded and that test starts failing so the conflict can't
// ship silently.
func StaticModelProviders() map[string]string {
	sources := [][]Model{
		claudeStaticModels(),
		codexStaticModels(),
		cursorStaticModels(),
		copilotStaticModels(),
		grokStaticModels(),
		codebuddyStaticModels(),
	}

	// First pass: collect every provider seen per id so a genuine
	// cross-catalog disagreement can be detected before anything is
	// committed to the result.
	providersByID := map[string]map[string]struct{}{}
	for _, models := range sources {
		for _, m := range models {
			if m.Provider == "" {
				continue
			}
			id := strings.ToLower(m.ID)
			if id == "" {
				continue
			}
			if providersByID[id] == nil {
				providersByID[id] = map[string]struct{}{}
			}
			providersByID[id][m.Provider] = struct{}{}
		}
	}

	result := make(map[string]string, len(providersByID))
	for id, providers := range providersByID {
		if len(providers) != 1 {
			// Conflicting provider claims for the same id across static
			// catalogs — exclude rather than guess. See the doc comment
			// above.
			continue
		}
		for provider := range providers {
			result[id] = provider
		}
	}
	return result
}
