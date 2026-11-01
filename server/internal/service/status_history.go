package service

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// StatusCauseUIOrAPI is the cause of a status move a person or client made
// through the issue API.
const StatusCauseUIOrAPI = "ui_or_api"

// StatusTriggerVersion is the schema version of details.trigger.
const StatusTriggerVersion = 1

type StatusCauseActor struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
}

type StatusCause struct {
	Kind string           `json:"kind"`
	By   StatusCauseActor `json:"by"`
}

// StatusTrigger is the trigger object stored in a status_changed row's
// details.trigger. Cause and AuthSource are server-set and trusted; Channel is
// a client hint and never authorizes anything. A nil AuthSource marks a
// background producer.
type StatusTrigger struct {
	Version          int         `json:"v"`
	Cause            StatusCause `json:"cause"`
	AuthSource       *string     `json:"auth_source"`
	Channel          string      `json:"channel"`
	HandoffCommentID string      `json:"handoff_comment_id,omitempty"`
}

// RecordStatusChange writes the status_changed activity row with qtx, the
// queries of the transaction that changes the status, so the row commits with
// the status or not at all. handoff entries are stored as handoff_<key>
// details, the way the activity listener stores them.
func RecordStatusChange(ctx context.Context, qtx *db.Queries, issue db.Issue, from, to string, trigger StatusTrigger, handoff map[string]string) (db.ActivityLog, error) {
	trigger.Version = StatusTriggerVersion
	details := map[string]any{"from": from, "to": to, "trigger": trigger}
	for k, v := range handoff {
		details["handoff_"+k] = v
	}
	body, err := json.Marshal(details)
	if err != nil {
		return db.ActivityLog{}, err
	}
	var actorID pgtype.UUID
	if trigger.Cause.By.ID != "" {
		if actorID, err = util.ParseUUID(trigger.Cause.By.ID); err != nil {
			return db.ActivityLog{}, err
		}
	}
	return qtx.CreateActivity(ctx, db.CreateActivityParams{
		ID:          dbid.NewV7(),
		WorkspaceID: issue.WorkspaceID,
		IssueID:     issue.ID,
		ActorType:   util.StrToText(trigger.Cause.By.Type),
		ActorID:     actorID,
		Action:      "status_changed",
		Details:     body,
	})
}
