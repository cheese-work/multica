package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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

// RunBudgetReceipt is what an executed raise or top-up did and where its run
// stands now (CHE-2864 A3). The numbers and NewWindow come from the executor's
// snapshot, so they are absent for an action executed before it existed.
type RunBudgetReceipt struct {
	Outcome        string `json:"outcome"`
	AppliedAt      string `json:"applied_at"`
	RunsUsed       *int64 `json:"runs_used,omitempty"`
	RunsLimit      *int64 `json:"runs_limit,omitempty"`
	RunsLimitAfter *int64 `json:"runs_limit_after,omitempty"`
	NewWindow      *bool  `json:"new_window,omitempty"`
}

// runBudgetReceipt is nil unless the action is an executed raise or top-up. A
// query error omits the receipt: the card still renders, so it logs at Info.
func runBudgetReceipt(ctx context.Context, q *db.Queries, action db.DecisionAction) *RunBudgetReceipt {
	if action.State != "executed" || (action.ActionType != runBudgetRaiseType && action.ActionType != runBudgetTopUpType) {
		return nil
	}
	var stored struct {
		From           *int64 `json:"from"`
		To             *int64 `json:"to"`
		RunsUsed       *int64 `json:"runs_used"`
		RunsLimit      *int64 `json:"runs_limit"`
		RunsLimitAfter *int64 `json:"runs_limit_after"`
		NewWindow      *bool  `json:"new_window"`
		Resume         string `json:"resume"`
	}
	if err := json.Unmarshal(action.Result, &stored); err != nil {
		slog.InfoContext(ctx, "run budget receipt omitted", "action_id", uuidToString(action.ID), "error", err)
		return nil
	}
	outcome, err := runBudgetResumeOutcome(ctx, q, action.ID, stored.Resume)
	if err != nil {
		slog.InfoContext(ctx, "run budget receipt omitted", "action_id", uuidToString(action.ID), "error", err)
		return nil
	}
	receipt := &RunBudgetReceipt{Outcome: outcome, AppliedAt: timestampToString(action.ExecutedAt), RunsUsed: stored.RunsUsed, RunsLimit: stored.RunsLimit, RunsLimitAfter: stored.RunsLimitAfter, NewWindow: stored.NewWindow}
	if action.ActionType == runBudgetRaiseType {
		// A raise executed before the snapshot kept only from and to.
		if receipt.RunsLimit == nil {
			receipt.RunsLimit = stored.From
		}
		if receipt.RunsLimitAfter == nil {
			receipt.RunsLimitAfter = stored.To
		}
	}
	return receipt
}

// runBudgetResumeOutcome is where the run the approval was for stands, read from
// the last refusal it linked. With none linked, the executor's own note (an
// unknown_origin refusal no approval can link) answers, else the cap was applied.
func runBudgetResumeOutcome(ctx context.Context, q *db.Queries, actionID pgtype.UUID, resume string) (string, error) {
	ids, err := q.ListRunBudgetRefusalIDsByAction(ctx, actionID)
	if err != nil {
		return "", err
	}
	if len(ids) == 0 {
		if resume != "" {
			return resume, nil
		}
		return "cap_applied", nil
	}
	refusal, err := q.GetRunBudgetRefusal(ctx, ids[len(ids)-1])
	if err != nil {
		return "", err
	}
	switch refusal.State {
	case "run_queued":
		if refusal.TaskID.Valid {
			task, err := q.GetAgentTask(ctx, refusal.TaskID)
			if err != nil {
				return "", err
			}
			if task.StartedAt.Valid {
				return "run_started", nil
			}
		}
		return "run_queued", nil
	case "status_restored":
		return "status_restored", nil
	case "still_blocked", "superseded":
		return refusal.State + ":" + refusal.StateReason.String, nil
	}
	return "cap_applied", nil
}
