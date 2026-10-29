package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
)

// A person can raise one issue's run cap; nobody else can (CHE-2226).

func raiseBudget(t *testing.T, issueID string, body any, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := withURLParam(newRequest(http.MethodPost, "/api/issues/"+issueID+"/run-budget", body), "id", issueID)
	if mutate != nil {
		mutate(req)
	}
	w := httptest.NewRecorder()
	testHandler.RaiseIssueRunBudget(w, req)
	return w
}

func TestRaiseIssueRunBudgetLetsMemberLiftOneIssue(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 2226010, mergeEdgeSteps)
	issue := spendBudget(t, c, 2, "in_progress")
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue); !errors.Is(err, service.ErrRunBudgetExhausted) {
		t.Fatalf("run over the cap = %v, want ErrRunBudgetExhausted", err)
	}
	settle()
	c.enter(t, "in_progress")

	w := raiseBudget(t, c.issueID, map[string]any{"runs": 5}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("raise = %d: %s", w.Code, w.Body.String())
	}
	var got struct{ MaxRuns, PreviousMaxRuns int }
	var raw map[string]int
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	got.MaxRuns, got.PreviousMaxRuns = raw["max_runs"], raw["previous_max_runs"]
	if got.MaxRuns != 5 || got.PreviousMaxRuns != 2 {
		t.Fatalf("raise response = %v, want 2 -> 5", raw)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='run_budget_raised' AND actor_type='member' AND actor_id=$2`, c.issueID, testUserID); n != 1 {
		t.Fatalf("run_budget_raised activity rows by the member = %d, want 1", n)
	}

	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue); err != nil {
		t.Fatalf("run inside the raised cap refused: %v", err)
	}
}

func TestRaiseIssueRunBudgetRefusesAnAgent(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 2226011, mergeEdgeSteps)
	spendBudget(t, c, 2, "in_progress")
	agentID := seededReadyAgentID(t)
	taskID := createHandlerTestTaskForAgent(t, agentID)

	asAgent := func(r *http.Request) {
		r.Header.Set("X-Agent-ID", agentID)
		r.Header.Set("X-Task-ID", taskID)
	}
	if w := raiseBudget(t, c.issueID, map[string]any{"runs": 5}, asAgent); w.Code != http.StatusForbidden {
		t.Fatalf("agent raise = %d, want 403: %s", w.Code, w.Body.String())
	}
	asTokenAgent := func(r *http.Request) {
		r.Header.Set("X-Actor-Source", "task_token")
		r.Header.Set("X-Agent-ID", agentID)
	}
	if w := raiseBudget(t, c.issueID, map[string]any{"runs": 5}, asTokenAgent); w.Code != http.StatusForbidden {
		t.Fatalf("task-token agent raise = %d, want 403: %s", w.Code, w.Body.String())
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM issue WHERE id=$1 AND max_runs_override IS NOT NULL`, c.issueID); n != 0 {
		t.Fatal("a refused raise still stored an override")
	}
}

func TestRaiseIssueRunBudgetValidatesTheNewCap(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 2226012, mergeEdgeSteps)
	spendBudget(t, c, 2, "in_progress")

	for name, runs := range map[string]int{"equal to the cap": 2, "below the cap": 1, "zero": 0, "negative": -3, "over the limit": 100000} {
		if w := raiseBudget(t, c.issueID, map[string]any{"runs": runs}, nil); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: raise to %d = %d, want 400: %s", name, runs, w.Code, w.Body.String())
		}
	}
	if w := raiseBudget(t, c.issueID, map[string]any{"runs": 5}, nil); w.Code != http.StatusOK {
		t.Fatalf("raise to 5 = %d: %s", w.Code, w.Body.String())
	}
	if w := raiseBudget(t, c.issueID, map[string]any{"runs": 4}, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("lowering 5 -> 4 = %d, want 400 (a raise only goes up)", w.Code)
	}
}

func TestGetIssueRunBudgetReportsUsedLimitAndSuggestedRaise(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 2339010, mergeEdgeSteps)
	spendBudget(t, c, 2, "in_progress")
	settle()

	req := withURLParam(newRequest(http.MethodGet, "/api/issues/"+c.issueID+"/run-budget", nil), "id", c.issueID)
	w := httptest.NewRecorder()
	testHandler.GetIssueRunBudget(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", w.Code, w.Body.String())
	}
	var got service.RunBudgetSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Budgeted || got.RunsLimit != 2 || got.RunsUsed < 2 || got.RunsSource != "workflow" || got.RaisePreview == nil {
		t.Fatalf("snapshot = %+v, want a budgeted workflow limit of 2 spent, with a raise preview", got)
	}
}
