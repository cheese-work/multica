package handler

import "github.com/multica-ai/multica/server/internal/reviewindependence"

// review_independence (CHE-9319) is a governed setting: it changes only through
// the workspace.settings.patch card, so a generic settings save keeps the stored
// value, as for capability_routing.
const reviewIndependenceKey = reviewindependence.SettingsKey

func reconcileReviewIndependence(stored, incoming map[string]any) {
	delete(incoming, reviewIndependenceKey)
	if v, ok := stored[reviewIndependenceKey]; ok {
		incoming[reviewIndependenceKey] = v
	}
}
