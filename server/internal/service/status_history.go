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

// Causes of the background writers that move a status without a request:
// a merged PR (auto-complete or the GitHub merge hook) and a Qoder Cloud run.
const (
	StatusCausePRMerged = "pr_merged"
	StatusCauseQoder    = "qoder"
)

// StatusTriggerVersion is the schema version of details.trigger.
const StatusTriggerVersion = 1

type StatusCauseActor struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
}

type StatusCause struct {
	Kind string            `json:"kind"`
	By   StatusCauseActor  `json:"by"`
	Ref  map[string]string `json:"ref,omitempty"`
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

// BackgroundStatusTrigger is the trigger of a status move no request made: it
// has no credential, so auth_source stays null.
func BackgroundStatusTrigger(kind string, by StatusCauseActor, ref map[string]string) StatusTrigger {
	return StatusTrigger{Cause: StatusCause{Kind: kind, By: by, Ref: ref}, Channel: "unknown"}
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
	// The timeline names the PRs that completed an issue (MUL-7429).
	if prs := trigger.Cause.Ref["pr"]; trigger.Cause.Kind == StatusCausePRMerged && prs != "" {
		details["source"] = "pr_automation"
		details["pull_requests"] = prs
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
