package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// storedResult is decision_action.result of the action, as a generic map.
func (action pendingAction) storedResult(test *testing.T) map[string]any {
	test.Helper()
	var raw []byte
	dbfx.QueryRow(test, `SELECT result FROM decision_action WHERE id = $1`, action.actionID).Scan(&raw)
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		test.Fatalf("result %q: %v", raw, err)
	}
	return result
}

// spendRuns counts n runs of the issue in its open window.
func spendRuns(test *testing.T, action pendingAction, n int) {
	test.Helper()
	for range n {
		dbfx.Task(test, action.agentID, testutil.Cols{"issue_id": action.issueID, "status": "completed", "runtime_id": handlerTestRuntimeID(test)})
	}
}

// runsUsedNow is the runs the issue's budget shows, read the way the owner's card does.
func runsUsedNow(test *testing.T, issueID string) float64 {
	test.Helper()
	request := withURLParam(newRequest(http.MethodGet, "/api/issues/"+issueID+"/run-budget", nil), "id", issueID)
	var snapshot struct {
		RunsUsed float64 `json:"runs_used"`
	}
	testutil.Call(test, testHandler.GetIssueRunBudget, request).Want(http.StatusOK).JSON(&snapshot)
	return snapshot.RunsUsed
}

func wantResultNumber(test *testing.T, result map[string]any, key string, want float64) {
	test.Helper()
	if got, ok := result[key].(float64); !ok || got != want {
		test.Fatalf("result[%s] = %v; want %v (result %v)", key, result[key], want, result)
	}
}

// D1: the executor stores what the owner saw at approval, read before the change.
func TestRaiseApprovalStoresTheBudgetSnapshot(test *testing.T) {
	action := newPendingRaise(test, 30)
	spendRuns(test, action.pendingAction, 4)
	used := runsUsedNow(test, action.issueID)
	if used < 4 {
		test.Fatalf("fixture shows %v runs used; want at least the 4 spent", used)
	}
	action.approveAccepted(test)
	result := action.storedResult(test)
	wantResultNumber(test, result, "from", baseCap)
	wantResultNumber(test, result, "to", 30)
	wantResultNumber(test, result, "runs_used", used)
	wantResultNumber(test, result, "runs_limit", baseCap)
	wantResultNumber(test, result, "runs_limit_after", 30)
	if result["outcome"] != "cap_applied" {
		test.Fatalf("outcome = %v; want cap_applied", result["outcome"])
	}
	if _, found := result["resume"]; found {
		test.Fatalf("a raise with no unknown_origin row stored resume: %v", result)
	}
}

// newBudgetedTopUp is a pending top-up on an issue capped at baseCap runs. Its
// workflow has no steps, so the fixture does not depend on the workspace's PR merge setting.
func newBudgetedTopUp(test *testing.T) (action pendingAction, window string) {
	test.Helper()
	fixture, window := budgetedRequestFixture(test)
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

func TestTopUpApprovalStoresTheBudgetSnapshot(test *testing.T) {
	action, window := newBudgetedTopUp(test)
	spendRuns(test, action, 3)
	used := runsUsedNow(test, action.issueID)
	if used < 3 {
		test.Fatalf("fixture shows %v runs used; want at least the 3 spent", used)
	}
	action.approveAccepted(test)
	result := action.storedResult(test)
	if result["issue_id"] != action.issueID || result["window_id"] == "" || result["window_id"] == window {
		test.Fatalf("issue_id/window_id = %v/%v; want the issue and the new window", result["issue_id"], result["window_id"])
	}
	wantResultNumber(test, result, "runs_used", used)
	wantResultNumber(test, result, "runs_limit", baseCap)
	wantResultNumber(test, result, "runs_limit_after", baseCap)
	if result["outcome"] != "cap_applied" || result["new_window"] != true {
		test.Fatalf("outcome/new_window = %v/%v; want cap_applied/true", result["outcome"], result["new_window"])
	}
}
