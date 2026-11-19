package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
)

// CHE-6345: an over-cap blocked issue runs no task of its own. A task on a
// sibling issue opens the raise card (CHE-6191), the person approves it with no
// task running on the target, and the cap rises and the over state clears.
func TestRaiseForOverCapBlockedIssueApprovedWithoutATaskClearsOver(test *testing.T) {
	taskOwner, target, window, body := crossIssueRaise(test)
	ctx := context.Background()
	// 15 runs spent against a cap of 10: balance -5, state over. Blocked.
	dbfx.Exec(test, `INSERT INTO issue_run_budget_entry (id, workspace_id, issue_id, window_id, kind, event_key, runs_delta, cap_delta)
		SELECT gen_random_uuid(), workspace_id, id, $2, 'run', 'run:over-'||id::text, -15, 0 FROM issue WHERE id = $1`, target.issueID, window)
	dbfx.Exec(test, `UPDATE issue SET status = 'blocked' WHERE id = $1`, target.issueID)
	// The fixture's own task on the target ends: nothing runs on the target.
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE issue_id = $1 AND status IN ('queued','dispatched','running')`, target.issueID)
	if got := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status IN ('queued','dispatched','running')`, target.issueID); got != 0 {
		test.Fatalf("target has %d live tasks, want none", got)
	}
	snapshot := func() service.RunBudgetSnapshot {
		issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(target.issueID))
		if err != nil {
			test.Fatal(err)
		}
		snap, err := service.LoadRunBudgetSnapshot(ctx, testHandler.Queries, issue)
		if err != nil {
			test.Fatal(err)
		}
		return snap
	}
	if before := snapshot(); before.RunsState != service.RunBudgetOver || before.RunsLimit != baseCap {
		test.Fatalf("before: state %q limit %d, want over and %d", before.RunsState, before.RunsLimit, baseCap)
	}

	body["params"] = target.raiseParams(window, 30)
	proposed := taskOwner.propose(test, body, http.StatusCreated)
	var actionID string
	dbfx.QueryRow(test, `SELECT id FROM decision_action WHERE decision_request_id = $1`, proposed.ID).Scan(&actionID)
	action := pendingAction{target, proposed.ID, actionID}
	if out := action.approveAccepted(test); out.State != "executed" {
		test.Fatalf("outcome = %+v, want executed", out)
	}
	after := snapshot()
	if after.RunsLimit != 30 || after.RunsState != service.RunBudgetOK || len(after.Blocking) != 0 {
		test.Fatalf("after: limit %d state %q blocking %v, want 30, ok, none", after.RunsLimit, after.RunsState, after.Blocking)
	}
}
