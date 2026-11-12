package handler

import (
	"context"
	"encoding/json"
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

// An executed or declined action already did its effect, so its platform
// comment starts no agent run (CHE-5287). The answer still counts as delivered.
// A failed action did not, so the asking agent still hears about it.
func TestActionAnswerStartsNoAgentRun(test *testing.T) {
	for _, outcome := range []string{"approved", "rejected", "failed"} {
		test.Run(outcome, func(test *testing.T) {
			resetDecisionLease(test)
			var action pendingAction
			switch outcome {
			case "approved":
				action, _ = newPendingTopUp(test, true)
			case "failed":
				action = newPendingSecretAction(test)
				restore := decisionActionExecutors["agent.env.patch"]
				decisionActionExecutors["agent.env.patch"] = func(context.Context, decisionActionExecution) (json.RawMessage, error) {
					return nil, actionFailure{Code: "boom", Message: "refused"}
				}
				test.Cleanup(func() { decisionActionExecutors["agent.env.patch"] = restore })
			default:
				action = newPendingAction(test)
			}
			dbfx.Exec(test, `DELETE FROM agent_task_queue WHERE issue_id = $1`, action.issueID)
			switch outcome {
			case "approved":
				action.approve(test, withVerifier(acceptingVerifier), action.claim(test, nil), http.StatusOK)
			case "failed":
				action.approve(test, withVerifier(acceptingVerifier), action.claim(test, map[string]string{"KEY": secretSentinel}), http.StatusOK)
			default:
				action.reject(test, testHandler, http.StatusOK)
			}
			_, runner := newDecisionRunnerFixture(test)
			decisionPass(test, runner)
			wantRuns, wantOutcome := 0, "skipped_ineligible"
			if outcome == "failed" {
				wantRuns, wantOutcome = 1, "run_enqueued"
			}
			if n := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, action.issueID); n != wantRuns {
				test.Fatalf("runs started by the %s action comment = %d, want %d", outcome, n, wantRuns)
			}
			if n := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1 AND woken_at IS NOT NULL AND wake_outcome = $2`, action.requestID, wantOutcome); n != 1 {
				test.Fatalf("settled card answers = %d, want 1", n)
			}
		})
	}
}
