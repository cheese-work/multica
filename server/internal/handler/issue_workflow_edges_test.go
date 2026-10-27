package handler

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// Level-triggered workflow edges (CHE-2152): entering a step with fact-guarded
// edges creates a watch, and the scheduler moves the issue when a guard
// holds, a deadline passes, or a PR closes without merging.

// edgeCase is a parent issue sitting in a project whose workflow has the
// given steps, with a child that carries the PR.
type edgeCase struct {
	fx         prMergeWebhookFixture
	workflowID string
	issueID    string
}

// newEdgeCase puts fx.parent in a project on a workflow with steps and leaves
// it at backlog; enter moves it onto a step as a person would.
func newEdgeCase(t *testing.T, installationID int64, steps []map[string]any) edgeCase {
	t.Helper()
	seedTestCatalog(t)
	fx := newPRMergeWebhookFixture(t, installationID)
	wf := createTestWorkflowSteps(t, "Edges "+workflowTestSuffix(), steps)
	projectID := createWorkflowTestProject(t, "Edges project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	if _, err := testPool.Exec(context.Background(),
		`UPDATE issue SET project_id=$2, status='backlog', assignee_type=NULL, assignee_id=NULL WHERE id=$1`, fx.parent.ID, projectID); err != nil {
		t.Fatal(err)
	}
	return edgeCase{fx: fx, workflowID: wf.ID, issueID: fx.parent.ID}
}

func (c edgeCase) enter(t *testing.T, status string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `UPDATE issue SET status=$2 WHERE id=$1`, c.issueID, status); err != nil {
		t.Fatal(err)
	}
}

func (c edgeCase) status(t *testing.T) string {
	t.Helper()
	s, _, _ := issueStatusAndAssignee(t, c.issueID)
	return s
}

func (c edgeCase) watches(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM issue_wakeup WHERE issue_id=$1 AND system_rule='workflow_edges'`, c.issueID)
}

// tick runs one scheduler pass, which evaluates every due watch.
func tickEdges(t *testing.T) {
	t.Helper()
	if err := (&service.IssueWakeupService{Tasks: testHandler.TaskService}).TickWorkspaces(context.Background(), parseUUID(testWorkspaceID)); err != nil {
		t.Fatalf("tick: %v", err)
	}
}

func setChildPR(t *testing.T, childID, state string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `UPDATE github_pull_request SET state=$2 WHERE id IN (SELECT pull_request_id FROM issue_pull_request WHERE issue_id=$1)`, childID, state); err != nil {
		t.Fatal(err)
	}
}

var mergeEdgeSteps = []map[string]any{
	{"status_key": "backlog"},
	{"status_key": "todo"},
	{"status_key": "in_progress", "edges": []map[string]any{
		{"to": "in_review", "guard": "pr.merged", "timeout": "72h", "on_timeout": "todo"},
	}},
	{"status_key": "in_review"},
	{"status_key": "blocked"},
}

func TestEdgesMergedBeforeEntryMovesOnFirstTick(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 2152001
	c := newEdgeCase(t, installationID, mergeEdgeSteps)
	linkedPRForWakeup(t, c.fx, installationID, 215201, "edge-a")
	setChildPR(t, c.fx.child.ID, "merged")
	c.enter(t, "in_progress")
	if got := c.watches(t); got != 1 {
		t.Fatalf("entering a step with a fact edge made %d watches, want 1", got)
	}
	tickEdges(t)
	if got := c.status(t); got != "in_review" {
		t.Fatalf("PR merged before entry left the issue at %s, want in_review", got)
	}
	if got := c.watches(t); got != 0 {
		t.Fatalf("a step without edges kept %d watches, want 0", got)
	}
}

func TestEdgesWaitThenTimeOutToOnTimeout(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 2152002
	c := newEdgeCase(t, installationID, mergeEdgeSteps)
	linkedPRForWakeup(t, c.fx, installationID, 215202, "edge-b")
	c.enter(t, "in_progress")
	tickEdges(t)
	if got := c.status(t); got != "in_progress" {
		t.Fatalf("open PR inside the timeout moved the issue to %s", got)
	}
	if got := c.watches(t); got != 1 {
		t.Fatalf("waiting issue has %d watches, want 1", got)
	}
	// Entered 100h ago: past the 72h deadline.
	testPool.Exec(context.Background(), `UPDATE issue_wakeup SET created_at=now()-interval '100 hours', next_fire_at=now() WHERE issue_id=$1 AND system_rule='workflow_edges'`, c.issueID)
	tickEdges(t)
	if got := c.status(t); got != "todo" {
		t.Fatalf("timed out issue is at %s, want on_timeout todo", got)
	}
}

func TestEdgesClosedUnmergedFallsBackToBlocked(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 2152003
	c := newEdgeCase(t, installationID, mergeEdgeSteps)
	linkedPRForWakeup(t, c.fx, installationID, 215203, "edge-c")
	c.enter(t, "in_progress")
	setChildPR(t, c.fx.child.ID, "closed")
	testPool.Exec(context.Background(), `UPDATE issue_wakeup SET next_fire_at=now() WHERE issue_id=$1 AND system_rule='workflow_edges'`, c.issueID)
	tickEdges(t)
	if got := c.status(t); got != "blocked" {
		t.Fatalf("PR closed without merging left the issue at %s, want blocked", got)
	}
}

func TestEdgesChildPRCountsUnlessRuleIsOff(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 2152004
	for _, tc := range []struct {
		name    string
		include bool
		want    string
	}{{"children count", true, "in_review"}, {"children ignored", false, "in_progress"}} {
		t.Run(tc.name, func(t *testing.T) {
			c := newEdgeCase(t, installationID+int64(map[bool]int{true: 0, false: 1}[tc.include]), mergeEdgeSteps)
			if _, err := testPool.Exec(context.Background(), `UPDATE issue_workflow SET include_child_prs=$2 WHERE id=$1`, c.workflowID, tc.include); err != nil {
				t.Fatal(err)
			}
			linkedPRForWakeup(t, c.fx, installationID+int64(map[bool]int{true: 0, false: 1}[tc.include]), 215204+int32(map[bool]int{true: 0, false: 1}[tc.include]), "edge-d")
			setChildPR(t, c.fx.child.ID, "merged")
			c.enter(t, "in_progress")
			tickEdges(t)
			if got := c.status(t); got != tc.want {
				t.Fatalf("include_child_prs=%v left the parent at %s, want %s", tc.include, got, tc.want)
			}
		})
	}
}

func TestEdgesNoWatchWithoutFactEdges(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 2152006, []map[string]any{
		{"status_key": "backlog"},
		{"status_key": "in_progress", "advance_on_ci_green": true, "next_status_key": "in_review"},
		{"status_key": "in_review"},
		{"status_key": "blocked"},
	})
	c.enter(t, "in_progress")
	if got := c.watches(t); got != 0 {
		t.Fatalf("legacy-only step made %d watches, want 0: the engine reads explicit edges only", got)
	}
}

func TestEdgesWatchEndsWhenIssueLeavesStep(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 2152007, mergeEdgeSteps)
	c.enter(t, "in_progress")
	if got := c.watches(t); got != 1 {
		t.Fatalf("watches = %d, want 1", got)
	}
	c.enter(t, "todo")
	if got := c.watches(t); got != 0 {
		t.Fatalf("leaving the step kept %d watches, want 0", got)
	}
}

// A watch whose issue sits on another status than the one it was written for
// is dropped without moving anything.
func TestEdgesStaleWatchIsDropped(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 2152008
	c := newEdgeCase(t, installationID, mergeEdgeSteps)
	linkedPRForWakeup(t, c.fx, installationID, 215208, "edge-e")
	setChildPR(t, c.fx.child.ID, "merged")
	c.enter(t, "in_progress")
	testPool.Exec(context.Background(), `UPDATE issue_wakeup SET condition_state='todo', next_fire_at=now() WHERE issue_id=$1 AND system_rule='workflow_edges'`, c.issueID)
	tickEdges(t)
	if got := c.status(t); got != "in_progress" {
		t.Fatalf("a stale watch moved the issue to %s", got)
	}
	if got := c.watches(t); got != 0 {
		t.Fatalf("stale watch kept: %d rows", got)
	}
}

func TestEdgesPausedProjectHoldsIssue(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 2152011
	c := newEdgeCase(t, installationID, mergeEdgeSteps)
	linkedPRForWakeup(t, c.fx, installationID, 215211, "edge-f")
	setChildPR(t, c.fx.child.ID, "merged")
	c.enter(t, "in_progress")
	testPool.Exec(context.Background(), `UPDATE project SET status='paused' WHERE id=(SELECT project_id FROM issue WHERE id=$1)`, c.issueID)
	testPool.Exec(context.Background(), `UPDATE issue_wakeup SET next_fire_at=now() WHERE issue_id=$1 AND system_rule='workflow_edges'`, c.issueID)
	tickEdges(t)
	if got := c.status(t); got != "in_progress" {
		t.Fatalf("paused project moved the issue to %s", got)
	}
	if got := c.watches(t); got != 1 {
		t.Fatalf("paused project left %d watches, want the watch kept", got)
	}
}

// A green head moves the issue once; back on the step at the same head it
// stays, because that head is spent.
func TestEdgesGreenEdgeAdvancesOncePerHead(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 2152012
	c := newEdgeCase(t, installationID, []map[string]any{
		{"status_key": "backlog"},
		{"status_key": "in_progress", "edges": []map[string]any{
			{"to": "in_review", "guard": "pr.checks_green", "timeout": "72h", "on_timeout": "todo"},
		}},
		{"status_key": "in_review"},
		{"status_key": "todo"},
		{"status_key": "blocked"},
	})
	linkedPRForWakeup(t, c.fx, installationID, 215212, "edge-g")
	testPool.Exec(context.Background(), `UPDATE github_pull_request SET checks_rollup_state='SUCCESS', snapshot_head_sha=head_sha WHERE workspace_id=$1 AND pr_number=215212`, testWorkspaceID)
	c.enter(t, "in_progress")
	tickEdges(t)
	if got := c.status(t); got != "in_review" {
		t.Fatalf("green head left the issue at %s, want in_review", got)
	}
	c.enter(t, "in_progress")
	testPool.Exec(context.Background(), `UPDATE issue_wakeup SET next_fire_at=now() WHERE issue_id=$1 AND system_rule='workflow_edges'`, c.issueID)
	tickEdges(t)
	if got := c.status(t); got != "in_progress" {
		t.Fatalf("a spent head advanced the issue again to %s", got)
	}
	// The spent head no longer blocks the deadline.
	testPool.Exec(context.Background(), `UPDATE issue_wakeup SET created_at=now()-interval '100 hours', next_fire_at=now() WHERE issue_id=$1 AND system_rule='workflow_edges'`, c.issueID)
	tickEdges(t)
	if got := c.status(t); got != "todo" {
		t.Fatalf("spent head with a passed deadline left the issue at %s, want on_timeout todo", got)
	}
}

// Hand-made wakeups on an issue in a watched step are not the engine's.
func TestEdgesLeaveHandMadeWakeupsAlone(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 2152013, mergeEdgeSteps)
	agentID := seededReadyAgentID(t)
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `INSERT INTO issue_wakeup (id, workspace_id, issue_id, agent_id, instruction, kind, mode, next_fire_at, timezone)
		VALUES (gen_random_uuid(), $1, $2, $3, 'hand made', 'at', 'once', now()+interval '1 day', 'UTC')`, testWorkspaceID, c.issueID, agentID); err != nil {
		t.Fatal(err)
	}
	c.enter(t, "in_progress")
	c.enter(t, "todo")
	c.enter(t, "in_progress")
	if n := dbfx.Count(t, `SELECT count(*) FROM issue_wakeup WHERE issue_id=$1 AND system_rule IS NULL`, c.issueID); n != 1 {
		t.Fatalf("hand-made wakeups = %d, want the 1 left untouched", n)
	}
}

func TestRunBudgetRefusesDerivedRunAndBlocksIssue(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 2152009, mergeEdgeSteps)
	agentID := seededReadyAgentID(t)
	ctx := context.Background()
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
	finish := func() {
		testPool.Exec(ctx, `UPDATE agent_task_queue SET status='completed',completed_at=clock_timestamp() WHERE issue_id=$1 AND status IN ('queued','dispatched','running')`, c.issueID)
	}
	for i := 0; i < 2; i++ {
		if _, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue); err != nil {
			t.Fatalf("run %d inside the budget refused: %v", i+1, err)
		}
		finish()
	}
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue); !errors.Is(err, service.ErrRunBudgetExhausted) {
		t.Fatalf("third run = %v, want ErrRunBudgetExhausted", err)
	}
	// The block runs after the refusal: status, then reason, then the
	// activity row. Wait for the last write before reading the others.
	activityRows := func() int {
		return dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='run_budget_exhausted'`, c.issueID)
	}
	deadline := time.Now().Add(5 * time.Second)
	for activityRows() == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if got := c.status(t); got != "blocked" {
		t.Fatalf("exhausted issue is %s, want blocked", got)
	}
	var reason string
	testPool.QueryRow(ctx, `SELECT COALESCE(metadata->>'blocked_reason','') FROM issue WHERE id=$1`, c.issueID).Scan(&reason)
	if reason != "run-budget" {
		t.Fatalf("blocked_reason = %q, want run-budget", reason)
	}
	if n := activityRows(); n != 1 {
		t.Fatalf("run_budget_exhausted activity rows = %d, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, c.issueID); n != 2 {
		t.Fatalf("issue has %d runs, want the 2 the budget allowed", n)
	}

	// A person moving the issue on clears the reason and starts a new budget.
	if _, err := testPool.Exec(ctx, `UPDATE issue SET status='in_progress' WHERE id=$1`, c.issueID); err != nil {
		t.Fatal(err)
	}
	testPool.QueryRow(ctx, `SELECT COALESCE(metadata->>'blocked_reason','') FROM issue WHERE id=$1`, c.issueID).Scan(&reason)
	if reason != "" {
		t.Fatalf("blocked_reason after leaving blocked = %q, want cleared", reason)
	}
}

func TestRunBudgetNamedRunBypasses(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 2152010, mergeEdgeSteps)
	agentID := seededReadyAgentID(t)
	ctx := context.Background()
	testPool.Exec(ctx, `UPDATE issue_workflow SET max_runs_per_issue=1, run_budget_since=now()-interval '1 hour' WHERE id=$1`, c.workflowID)
	testPool.Exec(ctx, `UPDATE issue SET status='in_progress' WHERE id=$1`, c.issueID)
	issue, _ := testHandler.Queries.GetIssue(ctx, parseUUID(c.issueID))
	if _, err := testHandler.TaskService.EnqueueTaskForMention(ctx, issue, parseUUID(agentID), pgtype.UUID{}, service.OriginNamed); err != nil {
		t.Fatalf("first named run: %v", err)
	}
	testPool.Exec(ctx, `UPDATE agent_task_queue SET status='completed',completed_at=clock_timestamp() WHERE issue_id=$1`, c.issueID)
	if _, err := testHandler.TaskService.EnqueueTaskForMention(ctx, issue, parseUUID(agentID), pgtype.UUID{}, service.OriginNamed); err != nil {
		t.Fatalf("a person naming the agent was refused by the budget: %v", err)
	}
}

func TestWorkflowRunRulesPersistAndPreserve(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	wf := createTestWorkflowSteps(t, "Rules "+workflowTestSuffix(), statusSteps("backlog", "in_progress", "blocked"))
	if wf.MaxRunsPerIssue != nil || !wf.IncludeChildPRs {
		t.Fatalf("new workflow rules = %v/%v, want default (nil/true)", wf.MaxRunsPerIssue, wf.IncludeChildPRs)
	}
	update := func(body map[string]any) IssueWorkflowResponse {
		t.Helper()
		var out IssueWorkflowResponse
		req := withURLParam(workflowRequest("PUT", "/api/issue-workflows/"+wf.ID, body), "id", wf.ID)
		testutil.Call(t, testHandler.UpdateIssueWorkflow, req).Want(http.StatusOK).JSON(&out)
		return out
	}
	got := update(map[string]any{"max_runs_per_issue": 7, "include_child_prs": false})
	if got.MaxRunsPerIssue == nil || *got.MaxRunsPerIssue != 7 || got.IncludeChildPRs {
		t.Fatalf("after set: %v/%v, want 7/false", got.MaxRunsPerIssue, got.IncludeChildPRs)
	}
	got = update(map[string]any{"name": "Renamed " + workflowTestSuffix()})
	if got.MaxRunsPerIssue == nil || *got.MaxRunsPerIssue != 7 || got.IncludeChildPRs {
		t.Fatalf("an update without rule fields changed them: %v/%v", got.MaxRunsPerIssue, got.IncludeChildPRs)
	}
	got = update(map[string]any{"max_runs_per_issue": 0})
	if got.MaxRunsPerIssue != nil {
		t.Fatalf("0 should clear to the default, got %v", *got.MaxRunsPerIssue)
	}
	req := withURLParam(workflowRequest("PUT", "/api/issue-workflows/"+wf.ID, map[string]any{"max_runs_per_issue": 1001}), "id", wf.ID)
	testutil.Call(t, testHandler.UpdateIssueWorkflow, req).Want(http.StatusBadRequest)
}
