package handler

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/multica-ai/multica/server/internal/queueorder"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// queueOrderFieldPatch gathers the queue fields a workspace update names into
// one settings patch. A JSON null is kept as nil, so the validator refuses it
// rather than the write dropping the key.
func queueOrderFieldPatch(order, aging json.RawMessage) map[string]any {
	patch := map[string]any{}
	for key, raw := range map[string]json.RawMessage{
		queueorder.KeyOrder: order,
		queueorder.KeyAging: aging,
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

// auditQueueOrderChange writes one audit row, in the caller's transaction,
// when a save moved queue_order or queue_aging_minutes: actor and time come
// from the audit path, old and new are the effective values. A save that
// leaves both effective values alone writes nothing.
func (h *Handler) auditQueueOrderChange(ctx context.Context, q *db.Queries, r *http.Request, before, after db.Workspace) error {
	oldValues, newValues := effectiveQueueOrder(before.Settings), effectiveQueueOrder(after.Settings)
	if oldValues[queueorder.KeyOrder] == newValues[queueorder.KeyOrder] && oldValues[queueorder.KeyAging] == newValues[queueorder.KeyAging] {
		return nil
	}
	return h.writeAudit(ctx, q, r, auditInput{
		Kind: "workspace.queue_order", WorkspaceID: after.ID, TargetID: after.ID,
		Before: oldValues, After: newValues,
	})
}

func effectiveQueueOrder(raw []byte) map[string]any {
	var settings map[string]any
	_ = json.Unmarshal(raw, &settings)
	return map[string]any{
		queueorder.KeyOrder: queueorder.Order(settings),
		queueorder.KeyAging: queueorder.AgingMinutes(settings),
	}
}
