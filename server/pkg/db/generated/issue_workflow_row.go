package db

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
)

// The issue_workflow queries name their columns instead of `*`, so no query
// text mentions the retired run_budget_token_* columns (CHE-5501; the columns
// drop in S5, CHE-5509). sqlc emits one row type per query for a column
// subset; these methods return the shared IssueWorkflow model, with the two
// retired fields left zero.

type (
	GetIssueWorkflowParams           = GetIssueWorkflowColsParams
	CreateIssueWorkflowParams        = CreateIssueWorkflowColsParams
	UpdateIssueWorkflowParams        = UpdateIssueWorkflowColsParams
	CreateDefaultIssueWorkflowParams = CreateDefaultIssueWorkflowColsParams
	LockDecisionWorkflowNowaitParams = LockDecisionWorkflowNowaitColsParams
)

// issueWorkflowCols is the field set every *Cols row type shares, so each
// converts to it directly.
type issueWorkflowCols struct {
	ID               pgtype.UUID        `json:"id"`
	WorkspaceID      pgtype.UUID        `json:"workspace_id"`
	Name             string             `json:"name"`
	Description      string             `json:"description"`
	InitialStatusKey string             `json:"initial_status_key"`
	Steps            []byte             `json:"steps"`
	CreatedAt        pgtype.Timestamptz `json:"created_at"`
	UpdatedAt        pgtype.Timestamptz `json:"updated_at"`
	IsDefault        bool               `json:"is_default"`
	OwnerID          pgtype.UUID        `json:"owner_id"`
	MaxRunsPerIssue  pgtype.Int4        `json:"max_runs_per_issue"`
	IncludeChildPrs  bool               `json:"include_child_prs"`
	RunBudgetSince   pgtype.Timestamptz `json:"run_budget_since"`
	StartStatusKey   pgtype.Text        `json:"start_status_key"`
}

func (c issueWorkflowCols) model() IssueWorkflow {
	return IssueWorkflow{
		ID: c.ID, WorkspaceID: c.WorkspaceID, Name: c.Name, Description: c.Description,
		InitialStatusKey: c.InitialStatusKey, Steps: c.Steps, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
		IsDefault: c.IsDefault, OwnerID: c.OwnerID, MaxRunsPerIssue: c.MaxRunsPerIssue,
		IncludeChildPrs: c.IncludeChildPrs, RunBudgetSince: c.RunBudgetSince, StartStatusKey: c.StartStatusKey,
	}
}

func (q *Queries) GetIssueWorkflow(ctx context.Context, arg GetIssueWorkflowParams) (IssueWorkflow, error) {
	row, err := q.GetIssueWorkflowCols(ctx, arg)
	return issueWorkflowCols(row).model(), err
}

func (q *Queries) GetDefaultIssueWorkflow(ctx context.Context, workspaceID pgtype.UUID) (IssueWorkflow, error) {
	row, err := q.GetDefaultIssueWorkflowCols(ctx, workspaceID)
	return issueWorkflowCols(row).model(), err
}

func (q *Queries) CreateIssueWorkflow(ctx context.Context, arg CreateIssueWorkflowParams) (IssueWorkflow, error) {
	row, err := q.CreateIssueWorkflowCols(ctx, arg)
	return issueWorkflowCols(row).model(), err
}

func (q *Queries) UpdateIssueWorkflow(ctx context.Context, arg UpdateIssueWorkflowParams) (IssueWorkflow, error) {
	row, err := q.UpdateIssueWorkflowCols(ctx, arg)
	return issueWorkflowCols(row).model(), err
}

func (q *Queries) LockDecisionWorkflowNowait(ctx context.Context, arg LockDecisionWorkflowNowaitParams) (IssueWorkflow, error) {
	row, err := q.LockDecisionWorkflowNowaitCols(ctx, arg)
	return issueWorkflowCols(row).model(), err
}

func (q *Queries) ListIssueWorkflows(ctx context.Context, workspaceID pgtype.UUID) ([]IssueWorkflow, error) {
	rows, err := q.ListIssueWorkflowsCols(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	out := make([]IssueWorkflow, len(rows))
	for i, r := range rows {
		out[i] = issueWorkflowCols(r).model()
	}
	return out, nil
}

func (q *Queries) CreateDefaultIssueWorkflow(ctx context.Context, arg CreateDefaultIssueWorkflowParams) ([]IssueWorkflow, error) {
	rows, err := q.CreateDefaultIssueWorkflowCols(ctx, arg)
	if err != nil {
		return nil, err
	}
	out := make([]IssueWorkflow, len(rows))
	for i, r := range rows {
		out[i] = issueWorkflowCols(r).model()
	}
	return out, nil
}
