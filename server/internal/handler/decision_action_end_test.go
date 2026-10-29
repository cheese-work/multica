package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func (action pendingAction) runLifecycle(test *testing.T) {
	test.Helper()
	if _, err := testHandler.CancelDecisionLifecycle(context.Background(), parseUUID(testWorkspaceID), parseUUID(action.requestID)); err != nil {
		test.Fatal(err)
	}
}

func (action pendingAction) activityCount(test *testing.T, name string) int {
	test.Helper()
	return dbfx.Count(test, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = $2 AND details->>'decision_action_id' = $3`, action.issueID, name, action.actionID)
}

func TestLifecycleCancelEndsThePendingAction(test *testing.T) {
	action := newPendingAction(test)
	dbfx.Exec(test, `UPDATE issue SET status = 'done' WHERE id = $1`, action.issueID)
	action.runLifecycle(test)
	if got := action.state(test); got != "cancelled" {
		test.Fatalf("action state = %s, want cancelled", got)
	}
	if action.activityCount(test, "decision_action_cancelled") != 1 {
		test.Fatal("no decision_action_cancelled activity")
	}
	action.approve(test, withVerifier(acceptingVerifier), action.claim(test, nil), http.StatusConflict)
}

func TestExpiredActionExpiresAndCancelsItsRequest(test *testing.T) {
	action := newPendingAction(test)
	dbfx.Exec(test, `UPDATE decision_action SET expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, action.actionID)
	if n := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'open'`, action.requestID); n != 1 {
		test.Fatal("request is not open")
	}
	action.runLifecycle(test)
	if got := action.state(test); got != "expired" {
		test.Fatalf("action state = %s, want expired", got)
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'cancelled' AND cancel_reason = 'expired'`, action.requestID); n != 1 {
		test.Fatal("request is not cancelled with reason expired")
	}
	if action.activityCount(test, "decision_action_expired") != 1 {
		test.Fatal("no decision_action_expired activity")
	}
}

func TestUnexpiredActionIsLeftToTheLifecycleScan(test *testing.T) {
	action := newPendingAction(test)
	action.runLifecycle(test)
	action.assertUntouched(test)
}

func TestAgentWithdrawEndsThePendingAction(test *testing.T) {
	action := newPendingAction(test)
	request := withURLParam(action.call(map[string]any{"reason": "no longer needed"}), "id", action.requestID)
	testutil.Call(test, testHandler.WithdrawDecision, request).Want(http.StatusOK)
	if got := action.state(test); got != "cancelled" {
		test.Fatalf("action state = %s, want cancelled", got)
	}
}
