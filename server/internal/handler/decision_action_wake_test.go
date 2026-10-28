package handler

import (
	"net/http"
	"testing"
)

// A card answer has no answering turn, so nothing finalizes it later: the
// click itself must make the answer schedulable, or the issue never wakes.
func TestRejectedActionAnswerIsScheduledForDelivery(test *testing.T) {
	resetDecisionLease(test)
	action := newPendingAction(test)
	// No live task for the issue, so delivery enqueues a fresh run.
	dbfx.Exec(test, `DELETE FROM agent_task_queue WHERE issue_id = (SELECT issue_id FROM decision_request WHERE id = $1)`, action.requestID)
	action.reject(test, testHandler, http.StatusOK)
	_, runner := newDecisionRunnerFixture(test)
	decisionPass(test, runner)
	if n := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1 AND answer_source = 'card' AND wake_at IS NOT NULL AND wake_at <= clock_timestamp()`, action.requestID); n != 1 {
		test.Fatalf("scheduled card answers = %d, want 1", n)
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1 AND woken_at IS NOT NULL`, action.requestID); n != 1 {
		test.Fatalf("delivered card answers = %d, want 1", n)
	}
}
