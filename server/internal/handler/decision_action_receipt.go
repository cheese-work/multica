package handler

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// runBudgetApprovalSnapshot is what the budget looked like when the owner
// approved, for the decision card's receipt (CHE-2864 A3). The executors call it
// under the issue lock, before they change the budget, and merge the keys into
// decision_action.result. runs_used and runs_limit are the values before the
// change; the raise executor overwrites runs_limit_after. When the window's
// refusal row is the unlinked unknown_origin one, resume records it: no later
// approval of the window can link that row, so the card needs the answer now.
func runBudgetApprovalSnapshot(ctx context.Context, q *db.Queries, issue db.Issue, windowID string) (map[string]any, error) {
	result := map[string]any{}
	snapshot, err := service.LoadRunBudgetSnapshot(ctx, q, issue)
	if err != nil {
		return nil, err
	}
	if snapshot.Budgeted {
		result["runs_used"], result["runs_limit"], result["runs_limit_after"] = snapshot.RunsUsed, snapshot.RunsLimit, snapshot.RunsLimit
	}
	refusal, err := q.GetRunBudgetRefusalByWindow(ctx, db.GetRunBudgetRefusalByWindowParams{IssueID: issue.ID, WindowID: windowID})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return nil, err
	case refusal.State == "superseded" && refusal.StateReason.String == stateReasonUnknownOrigin && !refusal.ActionID.Valid:
		result["resume"] = "superseded:" + stateReasonUnknownOrigin
	}
	return result, nil
}
