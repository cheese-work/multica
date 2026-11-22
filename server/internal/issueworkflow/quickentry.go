package issueworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// QuickWorkflowName is the name of the Quick workflow. Workflow names are
// unique per workspace without regard to case, so the name picks one workflow.
const QuickWorkflowName = "Quick"

// ActivityQuickEntry is the activity every bind into Quick writes (CHE-7681).
// Details: source ("arm" for the quick_first draw, "override" for an
// issue-level switch), arm ("Q" or "D", draw rows only) and full_workflow_id,
// the workflow the issue followed just before (omitted when it had none).
// The newest row is the one a later escalation reads.
const ActivityQuickEntry = "quick_entry"

// Sources of a quick_entry row.
const (
	QuickEntrySourceArm      = "arm"
	QuickEntrySourceOverride = "override"
)

// IsQuick reports whether def is the Quick workflow: the one named Quick, or
// the one the quick_first setting names (qf may be nil). The name alone
// identifies it before quick_first is set.
func IsQuick(def *Definition, qf *QuickFirst) bool {
	if def == nil {
		return false
	}
	if strings.EqualFold(def.Name, QuickWorkflowName) {
		return true
	}
	return qf != nil && def.ID.Valid && def.ID == qf.WorkflowID
}

// EntryQuerier is what recording a quick_entry reads and writes.
type EntryQuerier interface {
	Querier
	GetWorkspace(ctx context.Context, id pgtype.UUID) (db.Workspace, error)
	CreateActivity(ctx context.Context, arg db.CreateActivityParams) (db.ActivityLog, error)
}

// PriorRoute is the workflow an issue followed before an override into Quick:
// its own override, else the workspace title map, else its project's workflow.
// It is invalid for an issue with none of them. The server computes it; no
// caller names it.
func PriorRoute(ctx context.Context, q EntryQuerier, issue db.Issue) (pgtype.UUID, error) {
	if issue.WorkflowID.Valid {
		return issue.WorkflowID, nil
	}
	workspace, err := q.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return pgtype.UUID{}, err
	}
	if id, ok := WorkflowForTitle(workspace.Settings, issue.Title); ok {
		_, err := q.GetIssueWorkflow(ctx, db.GetIssueWorkflowParams{ID: id, WorkspaceID: issue.WorkspaceID})
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return pgtype.UUID{}, err
		}
	}
	if !issue.ProjectID.Valid {
		return pgtype.UUID{}, nil
	}
	def, _, err := ForIssue(ctx, q, issue.WorkspaceID, pgtype.UUID{}, issue.ProjectID)
	if err != nil || def == nil {
		return pgtype.UUID{}, err
	}
	return def.ID, nil
}

// RecordQuickEntry writes one quick_entry row. arm is "Q" or "D" for a draw
// row and empty for an override; actorType is empty for a server decision.
func RecordQuickEntry(ctx context.Context, q interface {
	CreateActivity(ctx context.Context, arg db.CreateActivityParams) (db.ActivityLog, error)
}, workspaceID, issueID pgtype.UUID, arm string, full pgtype.UUID, actorType string, actorID pgtype.UUID) error {
	details := map[string]string{"source": QuickEntrySourceOverride}
	if arm != "" {
		details["source"], details["arm"] = QuickEntrySourceArm, arm
	}
	if full.Valid {
		details["full_workflow_id"] = util.UUIDToString(full)
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return err
	}
	params := db.CreateActivityParams{WorkspaceID: workspaceID, IssueID: issueID, Action: ActivityQuickEntry, Details: raw}
	if actorType != "" {
		params.ActorType, params.ActorID = pgtype.Text{String: actorType, Valid: true}, actorID
	}
	_, err = q.CreateActivity(ctx, params)
	return err
}
