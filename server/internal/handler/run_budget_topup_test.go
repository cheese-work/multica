package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
)

// A member top-up opens a new run-budget window (CHE-2297 L2d); an agent's
// task token cannot.

func topUpBudget(t *testing.T, issueID string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := withURLParam(newRequest(http.MethodPost, "/api/issues/"+issueID+"/run-budget/top-up", nil), "id", issueID)
	if mutate != nil {
		mutate(req)
	}
	w := httptest.NewRecorder()
	testHandler.TopUpIssueRunBudget(w, req)
	return w
}

func TestTopUpIssueRunBudgetOpensAWindow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 2316010, mergeEdgeSteps)
	issue := spendBudget(t, c, 2, "in_progress")
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue); !errors.Is(err, service.ErrRunBudgetExhausted) {
		t.Fatalf("run over the cap = %v, want ErrRunBudgetExhausted", err)
	}
	settle()

	if w := topUpBudget(t, c.issueID, nil); w.Code != http.StatusOK {
		t.Fatalf("top-up = %d: %s", w.Code, w.Body.String())
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='run_budget_top_up' AND actor_type='member' AND actor_id=$2`, c.issueID, testUserID); n != 1 {
		t.Fatalf("run_budget_top_up activity rows by the member = %d, want 1", n)
	}
	// The window credit is the workflow cap again: both runs fit, a third does not.
	for i := 0; i < 2; i++ {
		if _, err := testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue); err != nil {
			t.Fatalf("run %d inside the new window refused: %v", i+1, err)
		}
		finishRuns(t, c)
	}
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue); !errors.Is(err, service.ErrRunBudgetExhausted) {
		t.Fatalf("run over the new window = %v, want ErrRunBudgetExhausted", err)
	}
}

func TestTopUpIssueRunBudgetRefusesATaskToken(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 2316011, mergeEdgeSteps)
	spendBudget(t, c, 2, "in_progress")
	agentID := seededReadyAgentID(t)
	taskID := createHandlerTestTaskForAgent(t, agentID)

	asTaskToken := func(r *http.Request) {
		r.Header.Set("X-Actor-Source", "task_token")
		r.Header.Set("X-Agent-ID", agentID)
		r.Header.Set("X-Task-ID", taskID)
	}
	if w := topUpBudget(t, c.issueID, asTaskToken); w.Code != http.StatusForbidden {
		t.Fatalf("task-token top-up = %d, want 403: %s", w.Code, w.Body.String())
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='run_budget_top_up'`, c.issueID); n != 0 {
		t.Fatal("a refused top-up still wrote an activity row")
	}
}
