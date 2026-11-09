package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// StatusCauseUIOrAPI is the cause of a status move a person or client made
// through the issue API.
const StatusCauseUIOrAPI = "ui_or_api"

// StatusCauseTaskReset is the cause of the background move that returns a
// stuck in_progress issue to todo after its run failed.
const StatusCauseTaskReset = "task_reset"

// StatusCauseWorkflowRemap is the cause of a status move a workflow edit or a
// project workflow switch made to fit an issue to the new workflow. It is the
// platform's administrative move, never a person's: it opens no run-budget
// window.
const StatusCauseWorkflowRemap = "workflow_remap"

// StatusCauseRunBudget is the cause of the block an exhausted run budget makes.
const StatusCauseRunBudget = "run_budget"

// Causes of the background writers that move a status without a request:
// a merged PR (auto-complete or the GitHub merge hook) and a Qoder Cloud run.
const (
	StatusCausePRMerged = "pr_merged"
	StatusCauseQoder    = "qoder"
)

// StatusCauseDecision is the cause of the move that parks an issue while it
// waits for a decision and of the move that restores it afterwards.
const StatusCauseDecision = "decision"

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
	// RefInvalid marks a handoff comment link that RecordStatusChange dropped.
	RefInvalid bool `json:"ref_invalid,omitempty"`
}

// BackgroundStatusTrigger is the trigger of a status move no request made: it
// has no credential, so auth_source stays null.
func BackgroundStatusTrigger(kind string, by StatusCauseActor, ref map[string]string) StatusTrigger {
	return StatusTrigger{Cause: StatusCause{Kind: kind, By: by, Ref: ref}, Channel: "unknown"}
}

// RecordStatusChange writes the status_changed activity row with qtx, the
// queries of the transaction that changes the status, so the row commits with
// the status or not at all. handoff entries are stored as handoff_<key>
// details, the way the activity listener stores them. A HandoffCommentID that
// is not a comment of the same workspace and issue is dropped and the trigger
// is marked RefInvalid.
func RecordStatusChange(ctx context.Context, qtx *db.Queries, issue db.Issue, from, to string, trigger StatusTrigger, handoff map[string]string) (db.ActivityLog, error) {
	return RecordStatusChangeWithEvent(ctx, qtx, issue, from, to, trigger, handoff, pgtype.UUID{}, false)
}

// RecordStatusChangeWithEvent is RecordStatusChange with a pre-assigned row id
// (a valid eventID; else a fresh UUIDv7) and, when routingExit is set, the
// details.routing_exit mark of a routing agent leaving its step.
func RecordStatusChangeWithEvent(ctx context.Context, qtx *db.Queries, issue db.Issue, from, to string, trigger StatusTrigger, handoff map[string]string, eventID pgtype.UUID, routingExit bool) (db.ActivityLog, error) {
	trigger, err := ResolveStatusTrigger(ctx, qtx, issue, trigger)
	if err != nil {
		return db.ActivityLog{}, err
	}
	details := map[string]any{"from": from, "to": to, "trigger": trigger}
	for k, v := range handoff {
		details["handoff_"+k] = v
	}
	if routingExit {
		details["routing_exit"] = true
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
	if !eventID.Valid {
		eventID = dbid.NewV7()
	}
	row, err := qtx.CreateActivity(ctx, db.CreateActivityParams{
		ID:          eventID,
		WorkspaceID: issue.WorkspaceID,
		IssueID:     issue.ID,
		ActorType:   util.StrToText(trigger.Cause.By.Type),
		ActorID:     actorID,
		Action:      "status_changed",
		Details:     body,
	})
	if err != nil {
		return row, err
	}
	return row, settleRefusalsOnStatusChange(ctx, qtx, issue, row, trigger)
}

// settleRefusalsOnStatusChange ties run-budget refusals to the status
// occurrence this row starts. The block a refusal's hook made is that refusal's
// occurrence. Any other move starts its own work and ends every non-terminal
// refusal of the issue with its unfinished hook; the platform's administrative
// remap only fits the issue to a new workflow, so it leaves them, and the hook's
// occurrence check retires one it made stale.
func settleRefusalsOnStatusChange(ctx context.Context, qtx *db.Queries, issue db.Issue, row db.ActivityLog, trigger StatusTrigger) error {
	switch {
	case trigger.Cause.Kind == StatusCauseRunBudget && trigger.Cause.Ref["refusal_id"] != "":
		id, err := util.ParseUUID(trigger.Cause.Ref["refusal_id"])
		if err != nil {
			return err
		}
		return qtx.MarkRunBudgetRefusalBlocked(ctx, db.MarkRunBudgetRefusalBlockedParams{ID: id, OccurrenceActivityID: row.ID})
	case trigger.Cause.Kind == StatusCauseWorkflowRemap:
		return nil
	}
	return qtx.SupersedeRunBudgetRefusalsOnStatusChange(ctx, issue.ID)
}

// ResetIssueForFailedTask moves an issue whose run failed back to todo and
// records the move, both with q. Callers with a transaction pass its queries so
// the row commits with the status.
func ResetIssueForFailedTask(ctx context.Context, q *db.Queries, issue db.Issue, task db.AgentTaskQueue) (db.Issue, db.ActivityLog, error) {
	updated, err := q.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{
		SourceTaskID: task.ID,
		ID:           issue.ID,
		Status:       "todo",
		WorkspaceID:  issue.WorkspaceID,
	})
	if err != nil {
		return db.Issue{}, db.ActivityLog{}, err
	}
	row, err := RecordStatusChange(ctx, q, updated, issue.Status, updated.Status, StatusTrigger{
		Cause: StatusCause{
			Kind: StatusCauseTaskReset,
			By:   StatusCauseActor{Type: "system"},
			Ref:  map[string]string{"run_id": util.UUIDToString(task.ID)},
		},
		Channel: "unknown",
	}, nil)
	if err != nil {
		return db.Issue{}, db.ActivityLog{}, fmt.Errorf("record status change: %w", err)
	}
	return updated, row, nil
}

// ResolveStatusTrigger stamps the trigger version and checks the handoff
// comment link against issue. A link that is not a comment of the same
// workspace and issue is dropped and the trigger is marked RefInvalid.
func ResolveStatusTrigger(ctx context.Context, qtx *db.Queries, issue db.Issue, trigger StatusTrigger) (StatusTrigger, error) {
	trigger.Version = StatusTriggerVersion
	trigger.RefInvalid = false
	if trigger.HandoffCommentID == "" {
		return trigger, nil
	}
	ok, err := handoffCommentBelongsTo(ctx, qtx, issue, trigger.HandoffCommentID)
	if err != nil {
		return trigger, err
	}
	if !ok {
		trigger.HandoffCommentID, trigger.RefInvalid = "", true
	}
	return trigger, nil
}

func handoffCommentBelongsTo(ctx context.Context, qtx *db.Queries, issue db.Issue, commentID string) (bool, error) {
	id, err := util.ParseUUID(commentID)
	if err != nil {
		return false, nil
	}
	c, err := qtx.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{ID: id, WorkspaceID: issue.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil && c.IssueID == issue.ID, err
}
