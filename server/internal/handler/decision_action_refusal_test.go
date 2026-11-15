package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func insertTestRefusal(test *testing.T, issueID, windowID string) string {
	test.Helper()
	id := uuid.NewString()
	dbfx.Exec(test, `INSERT INTO run_budget_refusal (id, workspace_id, issue_id, window_id) VALUES ($1, $2, $3, $4)`, id, testWorkspaceID, issueID, windowID)
	dbfx.Cleanup(test, `DELETE FROM run_budget_refusal WHERE id = $1`, id)
	return id
}

func TestApproveDecisionActionChecksTheRefusalIssueAndWindow(test *testing.T) {
	otherIssueID := dbfx.Issue(test, "other issue")
	cases := []struct {
		name    string
		refusal func(issueID string) string
		status  int
	}{
		{"matching refusal", func(issueID string) string { return insertTestRefusal(test, issueID, "w1") }, http.StatusOK},
		{"refusal of another issue", func(string) string { return insertTestRefusal(test, otherIssueID, "w1") }, http.StatusConflict},
		{"refusal of another window", func(issueID string) string { return insertTestRefusal(test, issueID, "w2") }, http.StatusConflict},
		{"unknown refusal", func(string) string { return uuid.NewString() }, http.StatusConflict},
	}
	for _, c := range cases {
		test.Run(c.name, func(test *testing.T) {
			fixture := newActionFixture(test)
			refusalID := c.refusal(fixture.issueID)
			params := map[string]any{"issue_id": fixture.issueID, "window_id": "w1", "refusal_id": refusalID}
			registerIssueNoopAction(test)
			var created actionResponse
			fixture.respond(test, fixture.withAction(map[string]any{"type": issueNoopAction, "version": 1, "params": params}), http.StatusCreated, &created)
			dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
			action := pendingAction{fixture, created.ID, created.Action.ID}
			body := action.approve(test, withVerifier(acceptingVerifier), action.claim(test, map[string]string{}), c.status)
			if c.status == http.StatusConflict {
				if !strings.Contains(body, "refusal_mismatch") {
					test.Fatalf("body %q; want refusal_mismatch", body)
				}
				action.assertUntouched(test)
			}
		})
	}
}
