package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type statusRecordKey struct{}

// statusRecord asks the issue write in ctx to record its status change in the
// write's own transaction. activity is the row it wrote, if the status moved.
type statusRecord struct {
	workspaceID pgtype.UUID
	trigger     service.StatusTrigger
	activity    *db.ActivityLog
}

func withStatusRecord(ctx context.Context, rec *statusRecord) context.Context {
	return context.WithValue(ctx, statusRecordKey{}, rec)
}

func statusRecordFrom(ctx context.Context) *statusRecord {
	rec, _ := ctx.Value(statusRecordKey{}).(*statusRecord)
	return rec
}

// statusTriggerFromRequest builds the trigger of a status move made through
// the issue API. X-Actor-Source is server-set; X-Client-Platform is a hint.
func statusTriggerFromRequest(r *http.Request, actorType, actorID string) service.StatusTrigger {
	trigger := service.StatusTrigger{
		Cause:   service.StatusCause{Kind: service.StatusCauseUIOrAPI, By: service.StatusCauseActor{Type: actorType, ID: actorID}},
		Channel: "unknown",
	}
	source := r.Header.Get("X-Actor-Source")
	switch source {
	case "interactive_jwt", "cli_jwt", "pat", "cloud_pat", "task_token":
		trigger.AuthSource = &source
	}
	switch platform := r.Header.Get("X-Client-Platform"); {
	case platform == "web" || platform == "desktop" || platform == "cli":
		trigger.Channel = platform
	case source == "cli_jwt" || source == "pat":
		trigger.Channel = "cli"
	}
	return trigger
}

// publishRecordedStatusActivity announces the row a migrated write committed,
// as the activity listener does for the rows it writes.
func (h *Handler) publishRecordedStatusActivity(rec *statusRecord, actorType, actorID string) {
	if rec == nil || rec.activity == nil {
		return
	}
	a := rec.activity
	h.publish(protocol.EventActivityCreated, uuidToString(a.WorkspaceID), actorType, actorID, map[string]any{
		"issue_id": uuidToString(a.IssueID),
		"entry": map[string]any{
			"type": "activity", "id": uuidToString(a.ID), "action": a.Action,
			"actor_type": a.ActorType.String, "actor_id": uuidToString(a.ActorID),
			"details": json.RawMessage(a.Details), "created_at": timestampToString(a.CreatedAt),
		},
	})
}

// recordSystemStatusChange writes the status_changed row of a system writer
// with the transaction's queries. It writes nothing when the status did not
// move, and then returns a nil row.
func recordSystemStatusChange(ctx context.Context, qtx *db.Queries, from, to db.Issue, kind string, ref map[string]string) (*db.ActivityLog, error) {
	if from.Status == to.Status {
		return nil, nil
	}
	trigger := service.BackgroundStatusTrigger(kind, service.StatusCauseActor{Type: "system"}, ref)
	row, err := service.RecordStatusChange(ctx, qtx, to, from.Status, to.Status, trigger, nil)
	if err != nil {
		return nil, fmt.Errorf("record status change: %w", err)
	}
	return &row, nil
}

// publishSystemStatusChange announces a committed system status move. A
// migrated write names its row, so the listener writes none of its own.
func (h *Handler) publishSystemStatusChange(workspaceID string, recorded *db.ActivityLog, payload map[string]any) {
	if recorded != nil {
		payload["activity_id"] = uuidToString(recorded.ID)
	}
	h.publish(protocol.EventIssueUpdated, workspaceID, "system", "", payload)
	h.publishRecordedStatusActivity(&statusRecord{activity: recorded}, "system", "")
}
