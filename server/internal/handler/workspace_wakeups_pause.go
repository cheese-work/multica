package handler

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
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
	// wakeupsResumedAtKey is stamped by the resume from the database clock and
	// owned by the server: a client snapshot never sets or loses it.
	wakeupsResumedAtKey = "wakeups_resumed_at"

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
	delete(incoming, wakeupsResumedAtKey)
	if v, ok := stored[wakeupsResumedAtKey]; ok {
		incoming[wakeupsResumedAtKey] = v
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

// settingsRejectedError is a settings body the pause guard refuses; the
// handler answers 400 instead of 500.
type settingsRejectedError struct{ msg string }

func (e settingsRejectedError) Error() string { return e.msg }

// saveWorkspace writes the update. A settings write runs in one transaction
// under the workspace row lock: the stored settings are read, the pause is
// reconciled against them, the row is written, and a resume settles what the
// pause held. The audit record of a flip commits with it, so a flip that
// cannot be audited does not happen. A write that cannot read the stored
// settings fails instead of guessing that they hold no pause.
func (h *Handler) saveWorkspace(ctx context.Context, params db.UpdateWorkspaceParams, settings any, pause *bool, userID, actorType, actorID string) (db.Workspace, string, db.ActivityLog, error) {
	var activity db.ActivityLog
	if settings == nil && pause == nil {
		ws, err := h.Queries.UpdateWorkspace(ctx, params)
		return ws, "", activity, err
	}
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return db.Workspace{}, "", activity, err
	}
	defer tx.Rollback(ctx)
	q := h.Queries.WithTx(tx)
	storedJSON, err := q.LockWorkspaceSettings(ctx, params.ID)
	if err != nil {
		return db.Workspace{}, "", activity, err
	}
	var stored map[string]any
	_ = json.Unmarshal(storedJSON, &stored) // stored settings that are not an object hold no pause
	action := ""
	if settings == nil {
		// A pause-only patch: start from what is stored, not from the client.
		clone := maps.Clone(stored)
		if clone == nil {
			clone = map[string]any{}
		}
		settings = clone
	}
	if incoming, isObject := settings.(map[string]any); isObject {
		if pause != nil {
			incoming[wakeupsPausedKey] = *pause
		}
		// Only an old client's flip of the retired PR switch needs the stored
		// value; see reconcilePRMergeSettings.
		reconcilePRMergeSettings(stored, incoming)
		reconcileDelegatedWrites(stored, incoming)
		if action, err = reconcileWakeupsPause(stored, incoming, userID, time.Now()); err != nil {
			return db.Workspace{}, "", activity, settingsRejectedError{err.Error()}
		}
	} else if stored[wakeupsPausedKey] == true || stored[delegatedWritesKey] != nil {
		// A non-object body would replace the whole blob and clear the pause or
		// the delegated-writes setting with no record. Other callers may still store one.
		return db.Workspace{}, "", activity, settingsRejectedError{"settings must be an object while wakeups are paused or delegated writes are set"}
	}
	params.Settings, _ = json.Marshal(settings)
	ws, err := q.UpdateWorkspace(ctx, params)
	if err != nil {
		return ws, "", activity, err
	}
	if action == wakeupsResumedAction {
		if err := (&service.IssueWakeupService{Tasks: h.TaskService}).SettleWakeupsOnResume(ctx, tx, ws.ID); err != nil {
			return ws, "", activity, err
		}
		// Return the row with the resume time the settle stamped.
		if ws, err = q.GetWorkspace(ctx, ws.ID); err != nil {
			return ws, "", activity, err
		}
	}
	if action != "" {
		activity, err = q.CreateActivity(ctx, db.CreateActivityParams{
			ID: dbid.NewV7(), WorkspaceID: ws.ID, ActorType: strToText(actorType), ActorID: optionalActorUUID(actorID),
			Action: action, Details: []byte(`{}`),
		})
		if err != nil {
			return ws, "", activity, err
		}
	}
	return ws, action, activity, tx.Commit(ctx)
}

// publishWakeupsPauseActivity announces a flip's committed audit record.
func (h *Handler) publishWakeupsPauseActivity(ws db.Workspace, actorType, actorID string, activity db.ActivityLog) {
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
