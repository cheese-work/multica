package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// settleProposal drives a proposed action to one terminal state and returns it.
// A proposal is the agent's own ask, so the agent is woken for every terminal
// state (CHE-5411), unlike a legacy card action (CHE-5287).
func settleProposal(test *testing.T, outcome string) pendingAction {
	test.Helper()
	resetDecisionLease(test)
	var action pendingAction
	switch outcome {
	case "applied":
		action, _ = newPendingTopUp(test, true)
	case "not_applied":
		action = newPendingSecretAction(test)
		restore := decisionActionExecutors["agent.env.patch"]
		decisionActionExecutors["agent.env.patch"] = func(context.Context, decisionActionExecution) (json.RawMessage, error) {
			return nil, actionFailure{Code: "boom", Message: "refused by the runtime"}
		}
		test.Cleanup(func() { decisionActionExecutors["agent.env.patch"] = restore })
	default:
		action = newPendingAction(test)
	}
	dbfx.Exec(test, `UPDATE decision_request SET proposal_fingerprint = 'p1:' || id::text WHERE id = $1`, action.requestID)
	dbfx.Exec(test, `DELETE FROM agent_task_queue WHERE issue_id = $1`, action.issueID)
	switch outcome {
	case "applied":
		action.approve(test, withVerifier(acceptingVerifier), action.claim(test, nil), http.StatusOK)
	case "not_applied":
		action.approve(test, withVerifier(acceptingVerifier), action.claim(test, map[string]string{"KEY": secretSentinel}), http.StatusOK)
	default:
		action.reject(test, testHandler, http.StatusOK)
	}
	return action
}

// The proposing agent hears of every terminal state exactly once; a second
// delivery pass changes nothing.
func TestProposedActionWakesTheAgentOncePerTerminalState(test *testing.T) {
	for _, outcome := range []string{"applied", "not_applied", "rejected"} {
		test.Run(outcome, func(test *testing.T) {
			action := settleProposal(test, outcome)
			_, runner := newDecisionRunnerFixture(test)
			decisionPass(test, runner)
			decisionPass(test, runner)
			if n := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, action.issueID); n != 1 {
				test.Fatalf("runs started for a %s proposal = %d, want 1", outcome, n)
			}
			if n := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1 AND woken_at IS NOT NULL AND wake_outcome = 'run_enqueued'`, action.requestID); n != 1 {
				test.Fatalf("woken card answers = %d, want 1", n)
			}
		})
	}
}

// The wake carries the outcome, and a failed apply carries its reason.
func TestProposedActionWakeStatesTheOutcomeAndReason(test *testing.T) {
	for outcome, want := range map[string]string{"applied": "Applied issue.run_budget.top_up.", "not_applied": "Not applied agent.env.patch: refused by the runtime", "rejected": "Rejected agent.env.patch."} {
		test.Run(outcome, func(test *testing.T) {
			action := settleProposal(test, outcome)
			var snapshot string
			dbfx.QueryRow(test, `SELECT content_snapshot FROM decision_answer WHERE decision_request_id = $1`, action.requestID).Scan(&snapshot)
			if !strings.Contains(snapshot, want) {
				test.Fatalf("wake text = %q, want it to contain %q", snapshot, want)
			}
		})
	}
}

// `multica decision get` reads applied_at, the outcome and the reason.
func TestGetDecisionReadsTheOutcome(test *testing.T) {
	for outcome, want := range map[string]struct{ outcome, reason string }{
		"applied": {"applied", ""}, "not_applied": {"not_applied", "refused by the runtime"}, "rejected": {"rejected", ""},
	} {
		test.Run(outcome, func(test *testing.T) {
			action := settleProposal(test, outcome)
			var read struct {
				AppliedAt     *string `json:"applied_at"`
				Outcome       string  `json:"outcome"`
				OutcomeReason *string `json:"outcome_reason"`
			}
			request := withURLParam(newRequest(http.MethodGet, "/api/decisions/"+action.requestID, nil), "id", action.requestID)
			testutil.Call(test, testHandler.GetDecision, request).Want(http.StatusOK).JSON(&read)
			if read.Outcome != want.outcome {
				test.Fatalf("outcome = %q, want %q", read.Outcome, want.outcome)
			}
			if (read.AppliedAt != nil) != (outcome == "applied") {
				test.Fatalf("applied_at = %v for %s", read.AppliedAt, outcome)
			}
			if got := ptrOr(read.OutcomeReason); got != want.reason {
				test.Fatalf("outcome_reason = %q, want %q", got, want.reason)
			}
		})
	}
}

func ptrOr(text *string) string {
	if text == nil {
		return ""
	}
	return *text
}
