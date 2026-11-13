package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

const topUpAction = "issue.run_budget.top_up"

// budgetFixtureIssue gives the fixture's issue a stored workflow budget whose
// window opened an hour ago, so a top-up has a window to replace.
func budgetFixtureIssue(test *testing.T, issueID string) {
	test.Helper()
	workflow := createTestWorkflowSteps(test, "Top-up "+workflowTestSuffix(), mergeEdgeSteps)
	dbfx.Exec(test, `UPDATE issue_workflow SET max_runs_per_issue = 3, run_budget_since = now() - interval '1 hour' WHERE id = $1`, workflow.ID)
	dbfx.Exec(test, `UPDATE issue SET workflow_id = $1 WHERE id = $2`, workflow.ID, issueID)
	dbfx.Cleanup(test, `DELETE FROM issue_run_budget_entry WHERE issue_id = $1`, issueID)
}

// windowID is the id the meter shows for the issue's open window.
func windowID(test *testing.T, issueID string) string {
	test.Helper()
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	if err != nil {
		test.Fatal(err)
	}
	snapshot, err := service.LoadRunBudgetSnapshot(context.Background(), testHandler.Queries, issue)
	if err != nil || !snapshot.Budgeted {
		test.Fatalf("snapshot budgeted=%v err=%v", snapshot.Budgeted, err)
	}
	return snapshot.WindowStartedAt.UTC().Format(time.RFC3339Nano)
}

func newPendingTopUp(test *testing.T, budgeted bool) (action pendingAction, window string) {
	test.Helper()
	fixture := newActionFixture(test)
	window = "2026-01-01T00:00:00Z"
	if budgeted {
		budgetFixtureIssue(test, fixture.issueID)
		window = windowID(test, fixture.issueID)
	}
	var created actionResponse
	fixture.respond(test, fixture.withAction(map[string]any{
		"type": topUpAction, "version": 1,
		"params": map[string]any{"issue_id": fixture.issueID, "window_id": window},
	}), http.StatusCreated, &created)
	dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
	action = pendingAction{fixture, created.ID, created.Action.ID}
	dbfx.Exec(test, `UPDATE member SET role = 'owner' WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, action.humanID)
	return action, window
}

func (action pendingAction) topUpRows(test *testing.T) int {
	test.Helper()
	return dbfx.Count(test, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'run_budget_top_up' AND actor_type = 'member' AND actor_id = $2 AND details->>'action_id' = $3`,
		action.issueID, action.humanID, action.actionID)
}

func (action pendingAction) failureCode(test *testing.T) string {
	test.Helper()
	var code string
	dbfx.QueryRow(test, `SELECT COALESCE(error_code, '') FROM decision_action WHERE id = $1`, action.actionID).Scan(&code)
	return code
}

func TestApproveRunBudgetTopUpOpensANewWindow(test *testing.T) {
	action, before := newPendingTopUp(test, true)
	action.approve(test, withVerifier(acceptingVerifier), action.claim(test, nil), http.StatusOK)
	if state := action.state(test); state != "executed" {
		test.Fatalf("action state = %s (%s), want executed", state, action.failureCode(test))
	}
	if rows := action.topUpRows(test); rows != 1 {
		test.Fatalf("run_budget_top_up rows by the approver = %d, want 1", rows)
	}
	after := windowID(test, action.issueID)
	if after == before {
		test.Fatalf("window id still %s after the top-up", before)
	}
	if opened := dbfx.Count(test, `SELECT count(*) FROM issue_run_budget_entry WHERE issue_id = $1 AND window_id = $2 AND kind = 'window'`, action.issueID, after); opened != 1 {
		test.Fatalf("ledger window entries for the new window = %d, want 1", opened)
	}
}

func TestApproveRunBudgetTopUpTwiceOpensOneWindow(test *testing.T) {
	action, _ := newPendingTopUp(test, true)
	action.approve(test, withVerifier(acceptingVerifier), action.claim(test, nil), http.StatusOK)
	action.approve(test, withVerifier(acceptingVerifier), action.claim(test, nil), http.StatusConflict)
	if rows := action.topUpRows(test); rows != 1 {
		test.Fatalf("run_budget_top_up rows after a duplicate approval = %d, want 1", rows)
	}
}

func TestApproveRunBudgetTopUpRefusesAChangedWindow(test *testing.T) {
	action, _ := newPendingTopUp(test, true)
	dbfx.Exec(test, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details) VALUES ($1, $2, 'member', $3, 'status_changed', '{"trigger":{"cause":{"kind":"ui_or_api"},"auth_source":"interactive_jwt"}}')`,
		testWorkspaceID, action.issueID, action.humanID)
	action.approve(test, withVerifier(acceptingVerifier), action.claim(test, nil), http.StatusOK)
	if state, code := action.state(test), action.failureCode(test); state != "failed" || code != "stale_window" {
		test.Fatalf("action state = %s code = %s, want failed / stale_window", state, code)
	}
	if rows := action.topUpRows(test); rows != 0 {
		test.Fatalf("a stale top-up wrote %d run_budget_top_up rows, want 0", rows)
	}
}

func TestApproveRunBudgetTopUpRefusesAnUnbudgetedIssue(test *testing.T) {
	action, _ := newPendingTopUp(test, false)
	action.approve(test, withVerifier(acceptingVerifier), action.claim(test, nil), http.StatusOK)
	if state, code := action.state(test), action.failureCode(test); state != "failed" || code != "not_budgeted" {
		test.Fatalf("action state = %s code = %s, want failed / not_budgeted", state, code)
	}
	if rows := action.topUpRows(test); rows != 0 {
		test.Fatalf("an unbudgeted top-up wrote %d run_budget_top_up rows, want 0", rows)
	}
}

func TestRequestRunBudgetTopUpRefusesAnotherIssueTarget(test *testing.T) {
	fixture := newActionFixture(test)
	other := dbfx.Issue(test, "other issue")
	body := fixture.respond(test, fixture.withAction(map[string]any{
		"type": topUpAction, "version": 1,
		"params": map[string]any{"issue_id": other, "window_id": "2026-01-01T00:00:00Z"},
	}), http.StatusBadRequest, nil)
	if !strings.Contains(body, "target_issue_mismatch") {
		test.Fatalf("body %q; want target_issue_mismatch", body)
	}
}

// A person's status move holds the boundary lock from before its status commit
// to after its activity write. An approval that read the window in that gap
// would top up the old window and then be followed by the move's own window.
func TestApproveRunBudgetTopUpWaitsForAMoveInFlight(test *testing.T) {
	action, _ := newPendingTopUp(test, true)
	release := holdRunBudgetBoundary(test, action.issueID)
	defer release()

	done := make(chan struct{})
	var body string
	go func() {
		defer close(done)
		raw, _ := json.Marshal(action.claim(test, nil))
		request := action.decide("approve", action.humanID, "interactive_jwt")
		request.Body = readerCloser{bytes.NewReader(raw)}
		body = testutil.Call(test, withVerifier(acceptingVerifier).ApproveDecisionAction, request).Want(http.StatusOK).Text()
	}()
	if !stillRunning(done) {
		test.Fatalf("the approval finished while a window boundary was in flight: %s", body)
	}
	// The move's activity write, which starts the next window.
	dbfx.Exec(test, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details) VALUES ($1, $2, 'member', $3, 'status_changed', '{"trigger":{"cause":{"kind":"ui_or_api"},"auth_source":"interactive_jwt"}}')`,
		testWorkspaceID, action.issueID, action.humanID)
	release()
	<-done
	if state, code := action.state(test), action.failureCode(test); state != "failed" || code != "stale_window" {
		test.Fatalf("action state = %s code = %s, want failed / stale_window", state, code)
	}
	if rows := action.topUpRows(test); rows != 0 {
		test.Fatalf("a stale top-up wrote %d run_budget_top_up rows, want 0", rows)
	}
}
