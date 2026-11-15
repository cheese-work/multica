package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// One test per status writer (CHE-2840, C1b1): the writer commits one
// status_changed row with its status, names it as activity_id in the event, and
// rolls the status back when the row cannot be written.

// recordActivityIDs collects the activity_id of every status-changing
// issue:updated event for issueID.
func recordActivityIDs(t *testing.T, issueID string) func() []string {
	t.Helper()
	var mu sync.Mutex
	var ids []string
	testHandler.Bus.Subscribe(protocol.EventIssueUpdated, func(e events.Event) {
		p, _ := e.Payload.(map[string]any)
		issue, _ := p["issue"].(IssueResponse)
		if changed, _ := p["status_changed"].(bool); !changed || issue.ID != issueID {
			return
		}
		id, _ := p["activity_id"].(string)
		mu.Lock()
		ids = append(ids, id)
		mu.Unlock()
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), ids...)
	}
}

func statusRowIDs(t *testing.T, issueID string) []string {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `SELECT id::text FROM activity_log WHERE issue_id = $1 AND action = 'status_changed' ORDER BY created_at, id`, issueID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

// wantOneRecordedRow fails unless issueID has exactly one status row with the
// given cause kind, and the event named that row.
func wantOneRecordedRow(t *testing.T, issueID, kind string, eventIDs []string) map[string]any {
	t.Helper()
	rows := statusRows(t, issueID)
	ids := statusRowIDs(t, issueID)
	if len(rows) != 1 || len(ids) != 1 {
		t.Fatalf("status_changed rows = %d, want 1", len(rows))
	}
	trigger, _ := rows[0]["trigger"].(map[string]any)
	cause, _ := trigger["cause"].(map[string]any)
	if cause["kind"] != kind {
		t.Fatalf("cause.kind = %v, want %s (trigger %v)", cause["kind"], kind, trigger)
	}
	if len(eventIDs) != 1 || eventIDs[0] != ids[0] {
		t.Fatalf("event activity_id = %v, want [%s]", eventIDs, ids[0])
	}
	return rows[0]
}

func batchUpdateStatus(issueID, status string) *http.Request {
	req := newRequest(http.MethodPost, "/api/issues/batch-update", map[string]any{
		"issue_ids": []string{issueID},
		"updates":   map[string]any{"status": status, "suppress_run": true},
	})
	req.Header.Set("X-Actor-Source", "interactive_jwt")
	req.Header.Set("X-Client-Platform", "web")
	return req
}

func TestBatchUpdateRecordsStatusHistoryInTransaction(t *testing.T) {
	issueID := createIssueForTimeline(t, "batch status history")
	events := recordActivityIDs(t, issueID)

	w := httptest.NewRecorder()
	testHandler.BatchUpdateIssues(w, batchUpdateStatus(issueID, "in_progress"))
	if w.Code != http.StatusOK {
		t.Fatalf("BatchUpdateIssues = %d: %s", w.Code, w.Body.String())
	}

	row := wantOneRecordedRow(t, issueID, "ui_or_api", events())
	trigger, _ := row["trigger"].(map[string]any)
	if trigger["auth_source"] != "interactive_jwt" || trigger["channel"] != "web" {
		t.Fatalf("trigger = %v", trigger)
	}
}

func TestBatchUpdateStatusRollsBackWithActivityFailure(t *testing.T) {
	issueID := createIssueForTimeline(t, "batch status rollback")
	failStatusActivityInserts(t, issueID)

	w := httptest.NewRecorder()
	testHandler.BatchUpdateIssues(w, batchUpdateStatus(issueID, "in_progress"))

	if status, _, _ := issueStatusAndAssignee(t, issueID); status != "todo" {
		t.Fatalf("status = %q after failed activity write (HTTP %d), want todo", status, w.Code)
	}
	if n := len(statusRowIDs(t, issueID)); n != 0 {
		t.Fatalf("status_changed rows = %d after rollback, want 0", n)
	}
}

func moveStatus(issueID, status string) *http.Request {
	req := withURLParam(newRequest(http.MethodPost, "/api/issues/"+issueID+"/move", map[string]any{
		"status": status, "before_id": nil, "after_id": nil,
	}), "id", issueID)
	req.Header.Set("X-Actor-Source", "interactive_jwt")
	req.Header.Set("X-Client-Platform", "desktop")
	return req
}

func TestMoveIssueRecordsStatusHistoryInTransaction(t *testing.T) {
	issueID := createIssueForTimeline(t, "move status history")
	ids := recordActivityIDs(t, issueID)

	testutil.Call(t, testHandler.MoveIssue, moveStatus(issueID, "in_progress")).Want(http.StatusOK)

	wantOneRecordedRow(t, issueID, "ui_or_api", ids())
}

// A move into a handoff step carries the handoff on the row, including the
// run the handoff started, which is only known after the commit.
func TestMoveIssueIntoHandoffStepRecordsHandoffOnTheRow(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	projectID := deliveryProject(t, "Move handoff history", agentID)
	issueID := dbfx.Issue(t, "move handoff history", testutil.Cols{"project_id": projectID, "status": "todo"})
	ids := recordActivityIDs(t, issueID)

	testutil.Call(t, testHandler.MoveIssue, moveStatus(issueID, "in_progress")).Want(http.StatusOK)

	row := wantOneRecordedRow(t, issueID, "ui_or_api", ids())
	if row["handoff_to_type"] != "agent" || row["handoff_to_id"] != agentID || row["handoff_run"] != "true" || row["handoff_workflow"] == nil {
		t.Fatalf("handoff details = %v, want the workflow, agent %s and run=true", row, agentID)
	}
}

func TestMoveIssueStatusRollsBackWithActivityFailure(t *testing.T) {
	issueID := createIssueForTimeline(t, "move status rollback")
	failStatusActivityInserts(t, issueID)

	testutil.Call(t, testHandler.MoveIssue, moveStatus(issueID, "in_progress")).Want(http.StatusInternalServerError)

	if status, _, _ := issueStatusAndAssignee(t, issueID); status != "todo" {
		t.Fatalf("status = %q after failed activity write, want todo", status)
	}
}

func TestWorkflowEdgeMoveRecordsStatusHistoryInTransaction(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 2840001, mergeEdgeSteps)
	linkedPRForWakeup(t, c.fx, 2840001, 284001, "edge-hist")
	setChildPR(t, c.fx.child.ID, "merged")
	c.enter(t, "in_progress")
	ids := recordActivityIDs(t, c.issueID)

	tickEdges(t)

	if got := c.status(t); got != "in_review" {
		t.Fatalf("edge left the issue at %s, want in_review", got)
	}
	row := wantOneRecordedRow(t, c.issueID, "workflow_edge", ids())
	trigger, _ := row["trigger"].(map[string]any)
	by, _ := trigger["cause"].(map[string]any)["by"].(map[string]any)
	if trigger["auth_source"] != nil || by["type"] != "system" {
		t.Fatalf("a background edge move has trigger %v, want no auth_source and a system actor", trigger)
	}
}

func TestWorkflowEdgeMoveRollsBackWithActivityFailure(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 2840002, mergeEdgeSteps)
	linkedPRForWakeup(t, c.fx, 2840002, 284002, "edge-roll")
	setChildPR(t, c.fx.child.ID, "merged")
	c.enter(t, "in_progress")
	failStatusActivityInserts(t, c.issueID)

	tickEdges(t)

	if got := c.status(t); got != "in_progress" {
		t.Fatalf("edge moved the issue to %s although its history row failed, want in_progress", got)
	}
	if n := len(statusRowIDs(t, c.issueID)); n != 0 {
		t.Fatalf("status_changed rows = %d after rollback, want 0", n)
	}
}

// ciAdvanceCase is a child on a project whose in_progress step advances on a
// green head, handing off to a reviewer.
func ciAdvanceCase(t *testing.T, installationID int64, prNumber int32, head string) (issueID, reviewer string, refresh func(), snap *greenSnapshot) {
	t.Helper()
	seedTestCatalog(t)
	fx := newPRMergeWebhookFixture(t, installationID)
	reviewer = seededReadyAgentID(t)
	wf := createTestWorkflowSteps(t, "CI history "+workflowTestSuffix(), []map[string]any{
		{"status_key": "backlog"},
		{"status_key": "in_progress", "advance_on_ci_green": true},
		{"status_key": "in_review", "handler": map[string]any{"type": "agent", "id": reviewer}, "instructions": "Review the PR."},
		{"status_key": "blocked"},
	})
	projectID := createWorkflowTestProject(t, "CI history project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	if _, err := testPool.Exec(context.Background(), `UPDATE issue SET project_id=$2, status='in_progress', assignee_type=NULL, assignee_id=NULL WHERE id=$1`, fx.child.ID, projectID); err != nil {
		t.Fatal(err)
	}
	linkedPRForWakeup(t, fx, installationID, prNumber, head)
	setWorkspacePRWakeSetting(t, "github_enabled", true)
	snap = newGreenSnapshot(head)
	refresh = startSnapshotRefresh(t, installationID, prNumber, snap.get)
	return fx.child.ID, reviewer, refresh, snap
}

func TestCIAdvanceRecordsStatusHistoryInTransaction(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	issueID, reviewer, refresh, _ := ciAdvanceCase(t, 2840003, 284003, "ci-hist-a")
	ids := recordActivityIDs(t, issueID)

	refresh()

	if status, _, _ := issueStatusAndAssignee(t, issueID); status != "in_review" {
		t.Fatalf("green head left the issue at %s, want in_review", status)
	}
	row := wantOneRecordedRow(t, issueID, "ci_advance", ids())
	if row["handoff_to_id"] != reviewer || row["handoff_run"] != "true" {
		t.Fatalf("handoff details = %v, want the reviewer %s and run=true", row, reviewer)
	}
}

func TestCIAdvanceRollsBackWithActivityFailure(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	issueID, _, refresh, _ := ciAdvanceCase(t, 2840004, 284004, "ci-hist-b")
	failStatusActivityInserts(t, issueID)

	refresh()

	if status, _, _ := issueStatusAndAssignee(t, issueID); status != "in_progress" {
		t.Fatalf("CI advance moved the issue to %s although its history row failed, want in_progress", status)
	}
	if n := len(statusRowIDs(t, issueID)); n != 0 {
		t.Fatalf("status_changed rows = %d after rollback, want 0", n)
	}
}

// exhaustRunBudget runs an issue on a two-run budget out of runs; the third
// enqueue is the refusal that blocks the issue.
func exhaustRunBudget(t *testing.T, c edgeCase) {
	t.Helper()
	ctx := context.Background()
	// Every Handler New() registers the process-wide exhaustion hook on its own
	// bus; point it back at testHandler so the block publishes where this
	// test listens.
	service.SetOnRunBudgetExhausted(testHandler.onRunBudgetExhausted)
	agentID := seededReadyAgentID(t)
	if _, err := testPool.Exec(ctx, `UPDATE issue_workflow SET max_runs_per_issue=2, run_budget_since=now()-interval '1 hour' WHERE id=$1`, c.workflowID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE issue SET status='in_progress', assignee_type='agent', assignee_id=$2 WHERE id=$1`, c.issueID, agentID); err != nil {
		t.Fatal(err)
	}
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue); err != nil {
			t.Fatalf("run %d inside the budget refused: %v", i+1, err)
		}
		testPool.Exec(ctx, `UPDATE agent_task_queue SET status='completed',completed_at=clock_timestamp() WHERE issue_id=$1 AND status IN ('queued','dispatched','running')`, c.issueID)
	}
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue); !errors.Is(err, service.ErrRunBudgetExhausted) {
		t.Fatalf("third run = %v, want ErrRunBudgetExhausted", err)
	}
}

func waitForExhaustedActivity(t *testing.T, issueID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='run_budget_exhausted'`, issueID) == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRunBudgetBlockRecordsStatusHistoryInTransaction(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 2840005, mergeEdgeSteps)
	ids := recordActivityIDs(t, c.issueID)

	exhaustRunBudget(t, c)
	waitForExhaustedActivity(t, c.issueID)

	if got := c.status(t); got != "blocked" {
		t.Fatalf("exhausted issue is %s, want blocked", got)
	}
	wantOneRecordedRow(t, c.issueID, "run_budget", ids())
}

func TestRunBudgetBlockRollsBackWithActivityFailure(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 2840006, mergeEdgeSteps)
	failStatusActivityInserts(t, c.issueID)

	exhaustRunBudget(t, c)
	time.Sleep(500 * time.Millisecond)

	if got := c.status(t); got != "in_progress" {
		t.Fatalf("budget block moved the issue to %s although its history row failed, want in_progress", got)
	}
	if n := len(statusRowIDs(t, c.issueID)); n != 0 {
		t.Fatalf("status_changed rows = %d after rollback, want 0", n)
	}
}
