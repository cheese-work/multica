package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// pendingBudgetAction asks for a real run-budget action on a budgeted issue,
// naming the refusal the case builds. The refusal row is made from the issue
// the action targets (or another one) and the window the action names.
func pendingBudgetAction(test *testing.T, typ string, refusal func(issueID, windowID string) string) pendingAction {
	test.Helper()
	var fixture singleCallFixture
	var windowID string
	switch typ {
	case raiseAction:
		fixture, windowID = budgetedRequestFixture(test)
	default:
		fixture = newActionFixture(test)
		budgetFixtureIssue(test, fixture.issueID)
		windowID = snapshotWindowID(test, fixture.issueID)
	}
	params := map[string]any{"issue_id": fixture.issueID, "window_id": windowID, "refusal_id": refusal(fixture.issueID, windowID)}
	if typ == raiseAction {
		params["new_cap"] = 30
	}
	var created actionResponse
	fixture.respond(test, fixture.withAction(map[string]any{"type": typ, "version": 1, "params": params}), http.StatusCreated, &created)
	dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
	action := pendingAction{fixture, created.ID, created.Action.ID}
	dbfx.Exec(test, `UPDATE member SET role = 'owner' WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, action.humanID)
	return action
}

func TestRunBudgetActionsApproveOnlyARefusalOfTheirOwnIssueAndWindow(test *testing.T) {
	otherIssueID := dbfx.Issue(test, "other issue")
	cases := []struct {
		name    string
		refusal func(issueID, windowID string) string
		status  int
	}{
		{"matching refusal", func(issueID, windowID string) string { return insertTestRefusal(test, issueID, windowID) }, http.StatusOK},
		{"refusal of another issue", func(_, windowID string) string { return insertTestRefusal(test, otherIssueID, windowID) }, http.StatusConflict},
		{"refusal of another window", func(issueID, _ string) string { return insertTestRefusal(test, issueID, "another-window") }, http.StatusConflict},
		{"unknown refusal", func(string, string) string { return uuid.NewString() }, http.StatusConflict},
	}
	for _, typ := range []string{raiseAction, topUpAction} {
		for _, c := range cases {
			test.Run(typ+"/"+c.name, func(test *testing.T) {
				action := pendingBudgetAction(test, typ, c.refusal)
				body := action.approve(test, withVerifier(acceptingVerifier), action.claim(test, map[string]string{}), c.status)
				if c.status == http.StatusOK {
					if state := action.state(test); state != "executed" {
						test.Fatalf("action state = %s (%s), want executed", state, action.failureCode(test))
					}
					return
				}
				if !strings.Contains(body, "refusal_mismatch") {
					test.Fatalf("body %q; want refusal_mismatch", body)
				}
				action.assertUntouched(test)
			})
		}
	}
}
