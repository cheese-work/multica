package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
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

func TestTopUpIssueRunBudgetRefusesACloudNodeCredential(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 2316012, mergeEdgeSteps)
	spendBudget(t, c, 2, "in_progress")

	// A cloud-node credential carries its owner's user ID and no agent headers,
	// so resolveActor alone would call it a member.
	asCloudPAT := func(r *http.Request) { r.Header.Set("X-Actor-Source", "cloud_pat") }
	if w := topUpBudget(t, c.issueID, asCloudPAT); w.Code != http.StatusForbidden {
		t.Fatalf("cloud-PAT top-up = %d, want 403: %s", w.Code, w.Body.String())
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='run_budget_top_up'`, c.issueID); n != 0 {
		t.Fatal("a refused top-up still wrote an activity row")
	}
}

// admitUntilRefused enqueues runs until the budget refuses one and returns how
// many it admitted: the balance as admission sees it, not as the ledger rows add up.
func admitUntilRefused(t *testing.T, c edgeCase, limit int) int {
	t.Helper()
	defer withoutRunBudgetWriters()()
	ctx := context.Background()
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	for admitted := 0; admitted < limit; admitted++ {
		if _, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue); errors.Is(err, service.ErrRunBudgetExhausted) {
			return admitted
		} else if err != nil {
			t.Fatalf("run %d: %v", admitted+1, err)
		}
		finishRuns(t, c)
	}
	return limit
}

// openLedger writes the issue's first window, as any admission does, so the
// runs left in it exist in the ledger before a member opens the next one.
func openLedger(t *testing.T, c edgeCase) {
	t.Helper()
	if n := admitUntilRefused(t, c, 1); n != 1 {
		t.Fatalf("first run refused")
	}
}

func TestTopUpCarriesTheRunsLeftIntoTheNewWindow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c, _ := budgetedAt(t, 40, 37)
	openLedger(t, c) // 2 runs left
	if w := topUpBudget(t, c.issueID, nil); w.Code != http.StatusOK {
		t.Fatalf("top-up = %d: %s", w.Code, w.Body.String())
	}
	if got := admitUntilRefused(t, c, 50); got != 42 {
		t.Fatalf("runs admitted after the top-up = %d, want 42 (2 left + a 40-run window)", got)
	}
}

func TestTopUpClearsAnOverdrawnWindow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	c, trigger := atZero(t) // cap 2, both spent
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	comment := dbfx.Comment(t, c.issueID, "@agent please look", testutil.Cols{})
	if _, err := testHandler.enqueueSingleCommentTrigger(ctx, issue, parseUUID(comment), trigger); err != nil {
		t.Fatalf("member mention at zero refused: %v", err) // balance -1
	}
	finishRuns(t, c)
	settle()
	if w := topUpBudget(t, c.issueID, nil); w.Code != http.StatusOK {
		t.Fatalf("top-up = %d: %s", w.Code, w.Body.String())
	}
	if got := admitUntilRefused(t, c, 10); got != 2 {
		t.Fatalf("runs admitted after the top-up = %d, want 2 (a fresh 2-run window; the overdraft is forgiven)", got)
	}
}

func TestRepeatedTopUpsStayAdditive(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 2805010, mergeEdgeSteps)
	spendBudget(t, c, 2, "in_progress") // cap 2, both spent
	if n := admitUntilRefused(t, c, 1); n != 0 {
		t.Fatalf("run over the cap admitted")
	}
	settle()
	for i := 0; i < 3; i++ {
		if w := topUpBudget(t, c.issueID, nil); w.Code != http.StatusOK {
			t.Fatalf("top-up %d = %d: %s", i+1, w.Code, w.Body.String())
		}
		settle()
	}
	if got := admitUntilRefused(t, c, 10); got != 6 {
		t.Fatalf("runs admitted after three top-ups = %d, want 6 (three 2-run windows)", got)
	}
}

// admitUntilRefused is setup. The warning and exhaustion writers its runs raise
// are goroutines that outlive the helper, bump the issue revision and read
// testHandler fields a later test swaps (CHE-3014).
func TestAdmitUntilRefusedLeavesNoLateIssueWriter(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 37)
	openLedger(t, c)
	if w := topUpBudget(t, c.issueID, nil); w.Code != http.StatusOK {
		t.Fatalf("top-up = %d: %s", w.Code, w.Body.String())
	}
	admitUntilRefused(t, c, 50)
	ctx := context.Background()
	before, err := testHandler.Queries.GetIssue(ctx, parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	after, err := testHandler.Queries.GetIssue(ctx, parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision {
		t.Fatalf("issue revision moved from %d to %d after admitUntilRefused returned: a late writer outlives the helper", before.Revision, after.Revision)
	}
}

// A top-up pays for a fresh window: debt an old window ran up does not leave
// the issue "over" with runs still unused (CHE-5331).
func TestTopUpIssueRunBudgetClearsCarriedDebt(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c, _ := budgetedAt(t, 40, 43)

	if w := topUpBudget(t, c.issueID, nil); w.Code != http.StatusOK {
		t.Fatalf("top-up = %d: %s", w.Code, w.Body.String())
	}
	snap := visibleSnapshot(t, c)
	if snap.RunsLeft != 40 || snap.State != service.RunBudgetOK || len(snap.Blocking) != 0 {
		t.Fatalf("after top-up: left=%d state=%q blocking=%v, want 40 left, ok, not blocked", snap.RunsLeft, snap.State, snap.Blocking)
	}
	admitRun(t, c)
	if got := ledgerBalance(t, c.issueID); got != 39 {
		t.Fatalf("balance after one admission = %d, want 39", got)
	}
}
