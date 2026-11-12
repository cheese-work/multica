package handler

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const runBudgetTopUpType = "issue.run_budget.top_up"

// executeRunBudgetTopUpAction opens a new run-budget window for the approver,
// like the direct top-up route. The approval holds the issue's boundary lock and
// its row; the window is read under them, so a status move or top-up that
// landed since the request ends the action instead of opening a second window.
func executeRunBudgetTopUpAction(ctx context.Context, execution decisionActionExecution) (json.RawMessage, error) {
	var params decisionaction.RunBudgetTopUpParams
	if err := json.Unmarshal(execution.Action.Params, &params); err != nil {
		return nil, err
	}
	issueID, err := parseUUIDString(params.IssueID)
	if err != nil {
		return nil, err
	}
	queries := execution.Queries
	current, err := service.LockRunBudgetWindow(ctx, queries, issueID)
	if err != nil {
		return nil, err
	}
	if current == "" {
		return nil, actionFailure{Code: "not_budgeted", Message: "the issue has no run budget to top up"}
	}
	if current != params.WindowID {
		return nil, actionFailure{Code: "stale_window", Message: "the run-budget window changed since the request; ask again"}
	}
	if err := service.ForgiveRunBudgetDebt(ctx, queries, issueID); err != nil {
		return nil, err
	}
	// The boundary is the time now, not the transaction's start, so it sorts
	// after every move that committed while this approval waited for its locks.
	startedAt, err := queries.RunBudgetBoundaryClock(ctx)
	if err != nil {
		return nil, err
	}
	details, err := json.Marshal(map[string]string{"action_id": uuidToString(execution.Action.ID)})
	if err != nil {
		return nil, err
	}
	if _, err := queries.CreateActivity(ctx, db.CreateActivityParams{
		WorkspaceID: execution.Action.WorkspaceID, IssueID: issueID,
		ActorType: pgtype.Text{String: "member", Valid: true}, ActorID: execution.Approver.UserID,
		Action: "run_budget_top_up", Details: details, CreatedAt: startedAt,
	}); err != nil {
		return nil, err
	}
	window, err := service.LockRunBudgetWindow(ctx, queries, issueID)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"issue_id": params.IssueID, "window_id": window})
}
