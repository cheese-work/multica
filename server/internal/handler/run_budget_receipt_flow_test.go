package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// approvedBudgetAction approves the run-budget card of an issue whose hook ran
// for real, and returns it. The card is the one a person gets: from the request
// endpoint, or (notice) the top-up card the refusal notice opened itself.
func approvedBudgetAction(test *testing.T, c budgetCase, how string) pendingAction {
	test.Helper()
	var requestID, actionID, humanID string
	if how == "notice" {
		dbfx.QueryRow(test, `SELECT r.id::text, a.id::text, r.human_user_id::text FROM decision_request r JOIN decision_action a ON a.decision_request_id = r.id WHERE r.issue_id = $1 AND a.action_type = $2`, c.issueID, runBudgetTopUpType).Scan(&requestID, &actionID, &humanID)
	} else {
		body := map[string]any{"kind": how, "window_id": snapshotWindowID(test, c.issueID)}
		if how == "raise" {
			body["new_cap"] = 30
		}
		request := withURLParam(newRequest(http.MethodPost, "/api/issues/"+c.issueID+"/run-budget/requests", body), "id", c.issueID)
		request.Header.Set("X-Actor-Source", "interactive_jwt")
		status := http.StatusCreated
		if how == "top_up" {
			status = http.StatusOK // the refusal notice opened this card already: the request returns it
		}
		var got budgetRequestResponse
		testutil.Call(test, RequireHumanActor(http.HandlerFunc(testHandler.RequestIssueRunBudgetAction)).ServeHTTP, request).Want(status).JSON(&got)
		requestID, actionID = got.RequestID, got.ActionID
		dbfx.QueryRow(test, `SELECT human_user_id::text FROM decision_request WHERE id = $1`, requestID).Scan(&humanID)
	}
	action := pendingAction{singleCallFixture{intentFixture: intentFixture{issueID: c.issueID, humanID: humanID}}, requestID, actionID}
	action.approveAccepted(test)
	return action
}

// A person approves a budget card on an issue that a block from before refusals
// were recorded stopped. The real hook left the refusal superseded:unknown_origin
// with no action, so the approval has no run to resume and the card says why
// (CHE-6947 D3b, AC11). Nothing here sets a refusal link or a resume by hand.
func TestRunBudgetReceiptOfAnApprovalOnAnUnknownOriginBlock(test *testing.T) {
	for index, how := range []string{"raise", "top_up", "notice"} {
		test.Run(how, func(test *testing.T) {
			c, e := hookCase(test, int64(6947300+index))
			c.enter(test, "blocked") // blocked by something that left no refusal row
			testHandler.onRunBudgetExhausted(context.Background(), e)
			if r := onlyRefusal(test, c); r.State != "superseded" || r.StateReason != "unknown_origin" {
				test.Fatalf("refusal = %+v, want superseded:unknown_origin", r)
			}
			action := approvedBudgetAction(test, c, how)
			if receipt := action.issueCardReceipt(test); receipt == nil || receipt.Outcome != "superseded:unknown_origin" {
				test.Fatalf("issue-cards receipt = %+v, want superseded:unknown_origin", receipt)
			}
			if r := onlyRefusal(test, c); r.State != "superseded" || r.StateReason != "unknown_origin" {
				test.Fatalf("refusal after approval = %+v, want it untouched", r)
			}
			if n := dbfx.Count(test, `SELECT count(*) FROM run_budget_refusal WHERE issue_id = $1 AND action_id IS NOT NULL`, c.issueID); n != 0 {
				test.Fatalf("refusals linked to an action = %d, want 0", n)
			}
		})
	}
}

// Control: a refusal superseded for any other reason has no run to resume either,
// but it is not an unknown origin, so the card reads the cap.
func TestRunBudgetReceiptOfAnApprovalOnAMovedRefusalIsCapApplied(test *testing.T) {
	c, e := hookCase(test, 6947310)
	testHandler.onRunBudgetExhausted(context.Background(), e)
	humanMove(test, c, "todo")
	if r := onlyRefusal(test, c); r.State != "superseded" || r.StateReason != "status_moved" {
		test.Fatalf("refusal = %+v, want superseded:status_moved", r)
	}
	action := approvedBudgetAction(test, c, "raise")
	if receipt := action.issueCardReceipt(test); receipt == nil || receipt.Outcome != "cap_applied" {
		test.Fatalf("issue-cards receipt = %+v, want cap_applied", receipt)
	}
}
