package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/dispatch"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The recovery reserve (CHE-2297 L3): the exhaustion move gives the blocked
// step's handler one run per budget window, and the server limits what that
// run can cause.

// recoverySteps is mergeEdgeSteps with an agent on the blocked step.
func recoverySteps(agentID string) []map[string]any {
	return []map[string]any{
		{"status_key": "backlog"},
		{"status_key": "todo"},
		{"status_key": "in_progress"},
		{"status_key": "in_review", "handler": map[string]any{"type": "agent", "id": agentID}, "instructions": "Review it."},
		{"status_key": "blocked", "handler": map[string]any{"type": "agent", "id": agentID}, "instructions": "Say why the run budget is spent."},
	}
}

func recoveryRuns(t *testing.T, c edgeCase) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND task_kind='budget_recovery'`, c.issueID)
}

// refuseOverCap spends the budget, then makes one more run be refused, which blocks
// the issue and, with a handler on the blocked step, claims the recovery run.
func refuseOverCap(t *testing.T, c edgeCase) {
	t.Helper()
	issue := spendBudget(t, c, 2, "in_progress")
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue); !errors.Is(err, service.ErrRunBudgetExhausted) {
		t.Fatalf("run over the cap = %v, want ErrRunBudgetExhausted", err)
	}
	waitForRunBudgetBlock(t, c)
}

func TestRecoveryRunExhaustionMoveEnqueuesOneAndASecondClaimIsRefused(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 2297301, recoverySteps(seededReadyAgentID(t)))
	refuseOverCap(t, c)
	settle()
	if got := c.status(t); got != "blocked" {
		t.Fatalf("issue is %s, want blocked", got)
	}
	for deadline := time.Now().Add(5 * time.Second); recoveryRuns(t, c) == 0 && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	if n := recoveryRuns(t, c); n != 1 {
		t.Fatalf("recovery runs after the exhaustion move = %d, want 1", n)
	}

	// Same window: put the issue back in progress, spend one more refusal.
	finishRuns(t, c)
	c.enter(t, "in_progress")
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue); !errors.Is(err, service.ErrRunBudgetExhausted) {
		t.Fatalf("second refusal = %v, want ErrRunBudgetExhausted", err)
	}
	settle()
	if got := c.status(t); got != "blocked" {
		t.Fatalf("issue is %s after the second refusal, want blocked again (the second claim must be attempted)", got)
	}
	if n := recoveryRuns(t, c); n != 1 {
		t.Fatalf("recovery runs after a second claim in the window = %d, want still 1", n)
	}
}

// agentCaller makes a request look like one made with an agent's task token:
// the headers Auth stamps. recovery adds what Auth adds for a recovery run's
// token, the context mark.
func agentCaller(t *testing.T, c edgeCase, recovery bool) func(*http.Request) *http.Request {
	t.Helper()
	agentID := seededReadyAgentID(t)
	taskID := createHandlerTestTaskForAgentOnIssue(t, agentID, c.issueID)
	if recovery {
		if _, err := testPool.Exec(context.Background(), `UPDATE agent_task_queue SET task_kind='budget_recovery' WHERE id=$1`, taskID); err != nil {
			t.Fatal(err)
		}
	}
	return func(r *http.Request) *http.Request {
		r.Header.Set("X-Actor-Source", "task_token")
		r.Header.Set("X-Agent-ID", agentID)
		r.Header.Set("X-Task-ID", taskID)
		if recovery {
			r = r.WithContext(dispatch.WithBudgetRecoveryCaller(r.Context()))
		}
		return r
	}
}

// roomyBudget is an issue with a handler on in_review and runs left in its
// window, so only the caller's identity decides whether a write starts a run
// or earns a grant.
func roomyBudget(t *testing.T, installationID int64) edgeCase {
	t.Helper()
	c := newEdgeCase(t, installationID, recoverySteps(seededReadyAgentID(t)))
	spendBudget(t, c, 2, "in_progress")
	if _, err := testPool.Exec(context.Background(), `UPDATE issue_workflow SET max_runs_per_issue=40 WHERE id=$1`, c.workflowID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(context.Background(), `DELETE FROM issue_run_budget_entry WHERE issue_id=$1`, c.issueID); err != nil {
		t.Fatal(err)
	}
	return c
}

func moveAs(t *testing.T, c edgeCase, as func(*http.Request) *http.Request, status string) {
	t.Helper()
	req := as(withURLParam(newRequest(http.MethodPut, "/api/issues/"+c.issueID, map[string]any{"status": status}), "id", c.issueID))
	w := httptest.NewRecorder()
	testHandler.UpdateIssue(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("move to %s = %d: %s", status, w.Code, w.Body.String())
	}
}

func queuedTaskCount(t *testing.T, c edgeCase) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND status='queued'`, c.issueID)
}

func stepGrants(t *testing.T, c edgeCase) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM issue_run_budget_entry WHERE issue_id=$1 AND kind='step'`, c.issueID)
}

// Negative control: the same move by an ordinary agent run earns a grant and
// starts the step handler's run, so the recovery test below can fail.
func TestRecoveryControlOrdinaryAgentMoveGrantsAndHandsOff(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := roomyBudget(t, 2297310)
	moveAs(t, c, agentCaller(t, c, false), "in_review")
	if n := stepGrants(t, c); n != 1 {
		t.Fatalf("step grants after an ordinary agent move = %d, want 1", n)
	}
	if n := queuedTaskCount(t, c); n != 1 {
		t.Fatalf("handoff runs after an ordinary agent move = %d, want 1", n)
	}
}

func TestRecoveryRunMoveToUnvisitedStepGetsNoGrantAndNoHandoffRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := roomyBudget(t, 2297311)
	moveAs(t, c, agentCaller(t, c, true), "in_review")
	if got := c.status(t); got != "in_review" {
		t.Fatalf("issue is %s, want the recovery run's move to land", got)
	}
	if n := stepGrants(t, c); n != 0 {
		t.Fatalf("step grants after a recovery-caused move = %d, want 0", n)
	}
	if n := queuedTaskCount(t, c); n != 0 {
		t.Fatalf("handoff runs after a recovery-caused move = %d, want 0", n)
	}
}

// otherReadyAgentID is a second agent that can run, to be named or assigned.
func otherReadyAgentID(t *testing.T) string {
	t.Helper()
	id := dbfx.Agent(t, "recovery bystander", dbfx.Runtime(t, "recovery bystander runtime"), testutil.Cols{
		"visibility": "workspace", "permission_mode": "public_to",
	})
	dbfx.Insert(t, "agent_invocation_target", testutil.Cols{"agent_id": id, "target_type": "workspace", "target_id": testWorkspaceID})
	return id
}

func TestRecoveryRunCommentMentionAndReplyStartNoRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	other := otherReadyAgentID(t)
	comment := func(t *testing.T, c edgeCase, as func(*http.Request) *http.Request, body map[string]any) {
		t.Helper()
		req := as(withURLParam(newRequest(http.MethodPost, "/api/issues/"+c.issueID+"/comments", body), "id", c.issueID))
		w := httptest.NewRecorder()
		testHandler.CreateComment(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("comment = %d: %s", w.Code, w.Body.String())
		}
	}
	mention := map[string]any{"content": "[@Other](mention://agent/" + other + ") please look"}

	control := roomyBudget(t, 2297312)
	comment(t, control, agentCaller(t, control, false), mention)
	if n := queuedTaskCount(t, control); n != 1 {
		t.Fatalf("control: runs started by an ordinary agent's mention = %d, want 1", n)
	}

	c := roomyBudget(t, 2297313)
	comment(t, c, agentCaller(t, c, true), mention)
	if n := queuedTaskCount(t, c); n != 0 {
		t.Fatalf("runs started by a recovery run's mention = %d, want 0", n)
	}

	// A reply in a thread that names the other agent starts it just as a top-level
	// mention does.
	reply := func(c edgeCase, recovery bool) {
		t.Helper()
		var parent string
		if err := testPool.QueryRow(context.Background(), `INSERT INTO comment(issue_id,workspace_id,author_type,author_id,content,type) VALUES($1,$2,'agent',$3,'what I found','comment') RETURNING id`,
			c.issueID, testWorkspaceID, other).Scan(&parent); err != nil {
			t.Fatal(err)
		}
		comment(t, c, agentCaller(t, c, recovery), map[string]any{"content": "[@Other](mention://agent/" + other + ") one more question", "parent_id": parent})
	}
	replyControl := roomyBudget(t, 2297320)
	reply(replyControl, false)
	if n := queuedTaskCount(t, replyControl); n != 1 {
		t.Fatalf("control: runs started by an ordinary agent's reply = %d, want 1", n)
	}
	r := roomyBudget(t, 2297321)
	reply(r, true)
	if n := queuedTaskCount(t, r); n != 0 {
		t.Fatalf("runs started by a recovery run's reply = %d, want 0", n)
	}
}

func TestRecoveryRunAssignmentAndRerunStartNoRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	other := otherReadyAgentID(t)
	c := roomyBudget(t, 2297314)
	as := agentCaller(t, c, true)

	assign := as(withURLParam(newRequest(http.MethodPut, "/api/issues/"+c.issueID, map[string]any{"assignee_type": "agent", "assignee_id": other}), "id", c.issueID))
	testHandler.UpdateIssue(httptest.NewRecorder(), assign)
	if n := queuedTaskCount(t, c); n != 0 {
		t.Fatalf("runs started by a recovery run's assignment = %d, want 0", n)
	}

	rerun := as(withURLParam(newRequest(http.MethodPost, "/api/issues/"+c.issueID+"/rerun", map[string]any{"reason": "recovery"}), "id", c.issueID))
	w := httptest.NewRecorder()
	testHandler.RerunIssue(w, rerun)
	if n := queuedTaskCount(t, c); n != 0 {
		t.Fatalf("runs started by a recovery run's rerun = %d (status %d), want 0", n, w.Code)
	}
}

func TestRecoveryRunWritesStillStartNoRunAfterAMemberTopUp(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := roomyBudget(t, 2297315)
	if w := topUpBudget(t, c.issueID, nil); w.Code != http.StatusOK {
		t.Fatalf("member top-up = %d: %s", w.Code, w.Body.String())
	}
	moveAs(t, c, agentCaller(t, c, true), "in_review")
	if n := queuedTaskCount(t, c); n != 0 {
		t.Fatalf("handoff runs by a recovery run after a member top-up = %d, want 0", n)
	}
	if n := stepGrants(t, c); n != 0 {
		t.Fatalf("step grants by a recovery run after a member top-up = %d, want 0", n)
	}
}

func TestRecoveryRunGets403OnChildCreationWakeupCreationAndTopUp(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := roomyBudget(t, 2297316)
	as := agentCaller(t, c, true)

	child := as(newRequest(http.MethodPost, "/api/issues", map[string]any{"title": "child of a recovery run", "parent_issue_id": c.issueID}))
	testutil.Call(t, testHandler.CreateIssue, child).Want(http.StatusForbidden)

	wake := as(withURLParam(newRequest(http.MethodPost, "/api/issues/"+c.issueID+"/wakeups",
		map[string]any{"agent_id": seededReadyAgentID(t), "kind": "at", "after_seconds": 600, "instruction": "check"}), "id", c.issueID))
	testutil.Call(t, testHandler.CreateIssueWakeup, wake).Want(http.StatusForbidden)

	if w := topUpBudget(t, c.issueID, func(r *http.Request) { *r = *as(r) }); w.Code != http.StatusForbidden {
		t.Fatalf("recovery run top-up = %d, want 403: %s", w.Code, w.Body.String())
	}
}

func TestRecoveryRunIsCancelledAfterTwentyMinutes(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := roomyBudget(t, 2297317)
	agentID := seededReadyAgentID(t)
	old := createHandlerTestTaskForAgentOnIssue(t, agentID, c.issueID)
	fresh := createHandlerTestTaskForAgentOnIssue(t, agentID, c.issueID)
	for id, age := range map[string]time.Duration{old: 21 * time.Minute, fresh: 5 * time.Minute} {
		if _, err := testPool.Exec(context.Background(), `UPDATE agent_task_queue SET task_kind='budget_recovery', created_at=now()-make_interval(secs => $2) WHERE id=$1`, id, age.Seconds()); err != nil {
			t.Fatal(err)
		}
	}
	testHandler.TaskService.CancelExpiredBudgetRecoveryRuns(context.Background())
	status := func(id string) string {
		var s string
		if err := testPool.QueryRow(context.Background(), `SELECT status FROM agent_task_queue WHERE id=$1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if got := status(old); got != "cancelled" {
		t.Fatalf("recovery run past 20 minutes is %s, want cancelled", got)
	}
	if got := status(fresh); got != "running" {
		t.Fatalf("recovery run at 5 minutes is %s, want running", got)
	}
}

func TestRecoveryFailedRunLeavesAMemberMoveAbleToAdmitRuns(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 2297318, recoverySteps(seededReadyAgentID(t)))
	refuseOverCap(t, c)
	settle()
	if _, err := testPool.Exec(context.Background(), `UPDATE agent_task_queue SET status='failed', completed_at=now() WHERE issue_id=$1 AND task_kind='budget_recovery'`, c.issueID); err != nil {
		t.Fatal(err)
	}
	if got := c.status(t); got != "blocked" {
		t.Fatalf("issue is %s after a failed recovery, want blocked", got)
	}
	memberMove(t, c)
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue); err != nil {
		t.Fatalf("run after a member move past a failed recovery refused: %v", err)
	}
}

// Auth marks a request made with a recovery run's task token, from the task's
// stored kind and nothing the client sends.
func TestAuthMarksARecoveryRunsTaskToken(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := roomyBudget(t, 2297319)
	agentID := seededReadyAgentID(t)
	callerMarked := func(kind string) bool {
		taskID := createHandlerTestTaskForAgentOnIssue(t, agentID, c.issueID)
		if kind != "" {
			if _, err := testPool.Exec(context.Background(), `UPDATE agent_task_queue SET task_kind=$2 WHERE id=$1`, taskID, kind); err != nil {
				t.Fatal(err)
			}
		}
		raw := "mat_" + uuid.NewString()
		if _, err := testHandler.Queries.CreateTaskToken(context.Background(), db.CreateTaskTokenParams{
			TokenHash: auth.HashToken(raw), TaskID: parseUUID(taskID), AgentID: parseUUID(agentID),
			WorkspaceID: parseUUID(testWorkspaceID), UserID: parseUUID(testUserID),
			ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		}); err != nil {
			t.Fatal(err)
		}
		marked := false
		router := chi.NewRouter()
		router.Use(middleware.Auth(testHandler.Queries, nil, nil, nil))
		router.Get("/probe", func(w http.ResponseWriter, r *http.Request) { marked = dispatch.BudgetRecoveryCaller(r.Context()) })
		req := httptest.NewRequest(http.MethodGet, "/probe", nil)
		req.Header.Set("Authorization", "Bearer "+raw)
		req.Header.Set("X-Workspace-ID", testWorkspaceID)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("probe with a %q task token = %d: %s", kind, w.Code, w.Body.String())
		}
		return marked
	}
	if callerMarked("") {
		t.Fatal("an ordinary task token is marked as a recovery caller")
	}
	if !callerMarked("budget_recovery") {
		t.Fatal("a recovery run's task token is not marked as a recovery caller")
	}
}
