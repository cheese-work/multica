package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// executeRunBudgetRaiseAction is the issue.run_budget.raise core: the cap, the
// run_budget_raised activity and the raise:action:<id> ledger entry commit with
// the approval or not at all. It runs under the issue lock the approval holds,
// so the checks below read the state the raise changes.
func executeRunBudgetRaiseAction(ctx context.Context, execution decisionActionExecution) (json.RawMessage, error) {
	var params decisionaction.RunBudgetRaiseParams
	if err := json.Unmarshal(execution.Action.Params, &params); err != nil {
		return nil, err
	}
	issueID, err := parseUUIDString(params.IssueID)
	if err != nil {
		return nil, err
	}
	queries := execution.Queries
	issue, err := queries.GetIssue(ctx, issueID)
	if err != nil {
		return nil, err
	}
	def, _, err := issueworkflow.ForIssue(ctx, queries, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID)
	if err != nil || def == nil {
		return nil, actionFailure{Code: "not_budgeted", Message: "the issue has no workflow run budget to raise"}
	}
	current, windowID, err := service.OpenRunBudgetWindowID(ctx, queries, issue.ID)
	if err != nil {
		return nil, err
	}
	if current == 0 {
		return nil, actionFailure{Code: "not_budgeted", Message: "the issue has no workflow run budget to raise"}
	}
	// A top-up or status move after the request opened a new window: the person
	// approved a raise for the old one.
	if windowID != params.WindowID {
		return nil, actionFailure{Code: "stale_window", Message: "the run budget window changed since this raise was requested; ask again"}
	}
	if params.NewCap <= current {
		return nil, actionFailure{Code: "stale_cap", Message: fmt.Sprintf("the cap is already %d; a raise never lowers it", current)}
	}
	raised, err := queries.RaiseIssueMaxRuns(ctx, db.RaiseIssueMaxRunsParams{
		ID: issue.ID, WorkspaceID: issue.WorkspaceID, Runs: int32(params.NewCap), WorkflowMax: int32(def.EffectiveMaxRuns()),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, actionFailure{Code: "stale_cap", Message: "the cap changed while raising it; ask again"}
	}
	if err != nil {
		return nil, err
	}
	details, err := json.Marshal(map[string]any{"from": current, "to": raised.Int32, "action_id": uuidToString(execution.Action.ID)})
	if err != nil {
		return nil, err
	}
	if _, err := queries.CreateActivity(ctx, db.CreateActivityParams{
		WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, ActorType: pgtype.Text{String: "member", Valid: true},
		ActorID: execution.Approver.UserID, Action: "run_budget_raised", Details: details,
	}); err != nil {
		return nil, err
	}
	if err := service.RecordRunBudgetActionRaise(ctx, queries, issue, execution.Action.ID, int(raised.Int32)-current); err != nil {
		return nil, err
	}
	// A raise keeps the window: the resume runs in the window the refusal named.
	if err := linkRunBudgetRefusal(ctx, queries, issue.ID, params.RefusalID, execution.Action.ID, windowID); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"from": current, "to": raised.Int32, "outcome": "cap_applied"})
}
