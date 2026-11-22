package handler

import (
	"encoding/json"

	"github.com/multica-ai/multica/server/internal/mergequeue"
)

// mergeQueueFieldPatch gathers the merge-queue fields a workspace update names
// into one settings patch. A JSON null is kept as nil, so the validator refuses
// it rather than the write dropping the key.
func mergeQueueFieldPatch(routing, bindings, watch json.RawMessage) map[string]any {
	patch := map[string]any{}
	for key, raw := range map[string]json.RawMessage{
		mergequeue.RoutingKey:       routing,
		mergequeue.ActorBindingsKey: bindings,
		mergequeue.WatchIssueKey:    watch,
	} {
		if raw == nil {
			continue
		}
		var value any
		_ = json.Unmarshal(raw, &value)
		patch[key] = value
	}
	return patch
}
