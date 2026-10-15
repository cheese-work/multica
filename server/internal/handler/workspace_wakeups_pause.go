package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Workspace wakeup pause (CHE-1727): the switch is three keys in
// workspace.settings. The server owns who and when; a client only sends the
// bool.
const (
	wakeupsPausedKey   = "wakeups_paused"
	wakeupsPausedByKey = "wakeups_paused_by"
	wakeupsPausedAtKey = "wakeups_paused_at"

	wakeupsPausedAction  = "wakeups_paused"
	wakeupsResumedAction = "wakeups_resumed"
)

var errWakeupsPausedNotBool = errors.New("wakeups_paused must be a boolean")

// reconcileWakeupsPause rewrites the pause keys of an incoming settings blob
// and returns the activity action when the switch flipped. A save that does not
// mention the switch keeps the stored state, so a client that never saw the
// pause cannot lift it; the stamp always comes from the server.
func reconcileWakeupsPause(stored, incoming map[string]any, actorID string, now time.Time) (string, error) {
	was := stored[wakeupsPausedKey] == true
	want := was
	if raw, sent := incoming[wakeupsPausedKey]; sent {
		b, ok := raw.(bool)
		if !ok {
			return "", errWakeupsPausedNotBool
		}
		want = b
	}
	delete(incoming, wakeupsPausedKey)
	delete(incoming, wakeupsPausedByKey)
	delete(incoming, wakeupsPausedAtKey)
	if !want {
		if was {
			return wakeupsResumedAction, nil
		}
		return "", nil
	}
	incoming[wakeupsPausedKey] = true
	if was {
		incoming[wakeupsPausedByKey], incoming[wakeupsPausedAtKey] = stored[wakeupsPausedByKey], stored[wakeupsPausedAtKey]
		return "", nil
	}
	incoming[wakeupsPausedByKey], incoming[wakeupsPausedAtKey] = actorID, now.UTC().Format(time.RFC3339)
	return wakeupsPausedAction, nil
}

// recordWakeupsPauseActivity writes the one audit record of a flip.
func (h *Handler) recordWakeupsPauseActivity(ctx context.Context, ws db.Workspace, actorType, actorID, action string) {
	activity, err := h.Queries.CreateActivity(ctx, db.CreateActivityParams{
		ID: dbid.NewV7(), WorkspaceID: ws.ID, ActorType: strToText(actorType), ActorID: optionalActorUUID(actorID),
		Action: action, Details: []byte(`{}`),
	})
	if err != nil {
		slog.Warn("record wakeups pause activity failed", "err", err, "workspace_id", uuidToString(ws.ID))
		return
	}
	h.publish(protocol.EventActivityCreated, uuidToString(ws.ID), actorType, actorID, map[string]any{
		"entry": map[string]any{
			"type": "activity", "id": uuidToString(activity.ID), "actor_type": actorType, "actor_id": actorID,
			"action": activity.Action, "details": json.RawMessage(`{}`), "created_at": timestampToString(activity.CreatedAt),
		},
	})
}

// wakeupResponse is a wakeup row plus the warnings the request earned.
type wakeupResponse struct {
	db.IssueWakeup
	Warnings []string `json:"warnings,omitempty"`
}

// wakeupWithWarnings tells the caller when a saved wakeup will not fire
// because its workspace is paused. The save itself still succeeds.
func (h *Handler) wakeupWithWarnings(ctx context.Context, row db.IssueWakeup) wakeupResponse {
	out := wakeupResponse{IssueWakeup: row}
	if paused, err := h.Queries.WorkspaceWakeupsPaused(ctx, row.WorkspaceID); err == nil && paused {
		out.Warnings = []string{"workspace_wakeups_paused"}
	}
	return out
}

