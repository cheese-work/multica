package issueworkflow

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// StepAssignee is one concrete assignee of a move: an agent, squad or member.
type StepAssignee struct {
	Type string
	ID   pgtype.UUID
}

// ResolveStepAssignee is the single writer of who a handoff move assigns, in
// this order: the assignee the move names, the issue's sticky assignee for the
// step, the step's handler. Naming or keeping the handler itself changes
// nothing. stepHandler is returned either way, so the status history can show
// what the step would have done. kept is true when the sticky assignee won.
func ResolveStepAssignee(stepHandler StepAssignee, override, sticky *StepAssignee) (assignee StepAssignee, overridden, kept bool) {
	switch {
	case override != nil:
		return *override, *override != stepHandler, false
	case sticky != nil && *sticky != stepHandler:
		return *sticky, true, true
	}
	return stepHandler, false, false
}

// StickyStepAssignee reads the issue's sticky assignee for a step; nil when
// there is none.
func StickyStepAssignee(ctx context.Context, q *db.Queries, workspaceID, issueID pgtype.UUID, statusKey string) (*StepAssignee, error) {
	row, err := q.GetIssueStepAssignee(ctx, db.GetIssueStepAssigneeParams{IssueID: issueID, StatusKey: statusKey, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &StepAssignee{Type: row.AssigneeType, ID: row.AssigneeID}, nil
}

// SetStepAssignee keeps who an issue goes to on every later entry of a step,
// replacing an earlier choice. Call it from the status transaction only.
func SetStepAssignee(ctx context.Context, q *db.Queries, workspaceID, issueID pgtype.UUID, statusKey string, assignee StepAssignee, setByType string, setByID pgtype.UUID) error {
	return q.UpsertIssueStepAssignee(ctx, db.UpsertIssueStepAssigneeParams{
		IssueID: issueID, StatusKey: statusKey, WorkspaceID: workspaceID,
		AssigneeType: assignee.Type, AssigneeID: assignee.ID,
		SetByType: setByType, SetByID: setByID,
	})
}

// ClearStepAssignee drops the sticky assignee of a step and reports whether
// there was one.
func ClearStepAssignee(ctx context.Context, q *db.Queries, workspaceID, issueID pgtype.UUID, statusKey string) (bool, error) {
	n, err := q.DeleteIssueStepAssignee(ctx, db.DeleteIssueStepAssigneeParams{IssueID: issueID, StatusKey: statusKey, WorkspaceID: workspaceID})
	return n > 0, err
}
