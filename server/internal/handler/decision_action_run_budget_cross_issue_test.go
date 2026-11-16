package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-6191: an agent task on issue A opens the raise card for an over-budget
// issue B. The card still needs a person's approval; nothing is applied.

// crossIssueRaise is a task on one issue and a budgeted target issue.
func crossIssueRaise(test *testing.T) (taskOwner, target singleCallFixture, window string, body map[string]any) {
	test.Helper()
	useProposalGuard(test)
	target, window = budgetedRequestFixture(test)
	dbfx.Cleanup(test, `DELETE FROM decision_action WHERE decision_request_id IN (SELECT id FROM decision_request WHERE issue_id = $1)`, target.issueID)
	taskOwner = otherIssueFixture(test, target)
	body = map[string]any{"issue_id": target.issueID, "action_type": raiseAction, "version": 1, "for": target.humanID, "params": target.raiseParams(window, baseCap+2)}
	return taskOwner, target, window, body
}

func TestProposalRaiseForAnotherIssueOpensACardAndAppliesNothing(test *testing.T) {
	taskOwner, target, window, body := crossIssueRaise(test)
	got := taskOwner.propose(test, body, http.StatusCreated)
	if got.Action == nil || got.Action.ActionType != raiseAction {
		test.Fatalf("action = %+v, want %s", got.Action, raiseAction)
	}
	// The card names both numbers: the cap now and the cap asked for.
	if !strings.Contains(got.Ask, "10") || !strings.Contains(got.Ask, "12") {
		test.Fatalf("ask must name the cap now (10) and the cap asked (12): %s", got.Ask)
	}
	var override *int
	dbfx.QueryRow(test, `SELECT max_runs_override FROM issue WHERE id = $1`, target.issueID).Scan(&override)
	if override != nil {
		test.Fatalf("issue cap override = %d before approval, want it untouched", *override)
	}
	if state := dbfx.Count(test, `SELECT count(*) FROM decision_action WHERE decision_request_id = $1 AND state = 'pending'`, got.ID); state != 1 {
		test.Fatalf("pending actions = %d, want 1", state)
	}
	// One open card per issue: a second ask returns it.
	body["params"] = target.raiseParams(window, baseCap+4)
	again := taskOwner.propose(test, body, http.StatusOK)
	if again.ID != got.ID {
		test.Fatalf("second ask opened %s, want the open card %s", again.ID, got.ID)
	}
}

func TestProposalOtherActionForAnotherIssueStaysRefused(test *testing.T) {
	registerIssueNoopAction(test)
	taskOwner, target, _, body := crossIssueRaise(test)
	body["action_type"], body["params"] = issueNoopAction, map[string]any{"issue_id": target.issueID}
	testutil.Call(test, testHandler.ProposeDecisionAction, taskOwner.proposeRequest(body)).Want(http.StatusForbidden)
}

func TestProposalRaiseWithoutATaskStaysRefused(test *testing.T) {
	_, target, _, body := crossIssueRaise(test)
	testutil.Call(test, testHandler.ProposeDecisionAction, newRequest(http.MethodPost, proposalPath, body)).Want(http.StatusForbidden)
	if target.proposalRows(test) != 0 {
		test.Fatal("row written without a task")
	}
}
