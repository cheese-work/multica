package handler

import (
	"net/http"
	"testing"
)

// CHE-7671 (CHE-7658 step 6): one close check, closeOrDispatch, runs before a
// handler run is enqueued on the status-write dispatch, the edge transaction and
// the pr.merged bridge. It answers closed, pending_merge or dispatch. These
// tests keep the real workflow_edges watcher in place: the trigger creates it on
// entry to a step whose edge reads PR facts.

var closeOrDispatchSteps = []map[string]any{
	{"status_key": "in_progress"},
	{"status_key": "agent_review", "edges": []map[string]any{
		{"to": "in_progress", "guard": "agent_verdict(CHANGES)"},
		{"to": "agent_accepted", "guard": "agent_verdict(PASS)"},
	}},
	{"status_key": "agent_accepted", "edges": []map[string]any{
		{"to": "done", "guard": "pr.merged", "timeout": "2h", "on_timeout": "agent_review"},
	}},
	{"status_key": "blocked"},
	{"status_key": "done"},
}

// newCloseOrDispatch is newAutoClose on a workflow whose agent_accepted step
// waits for the merge with its own watcher. prMergeStatus is the workspace
// setting: none makes the edge evaluator move the issue, a status makes the
// bridge hand the merge to the PR merge decision.
func newCloseOrDispatch(t *testing.T, mode, prMergeStatus string) autoClose {
	t.Helper()
	// A chosen status refuses a new pr.merged edge, so the workflow is saved with none.
	setWorkspacePRMergeStatusForTest(t, "none")
	c := newAutoClose(t, mode)
	wf := createTestWorkflowSteps(t, "Close or dispatch "+workflowTestSuffix(), closeOrDispatchSteps)
	setProjectWorkflow(t, c.projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	setColumnHandoff(t, c.projectID, "agent_accepted", handToAgent(c.acceptor, "Accept it.")).Want(http.StatusOK)
	if prMergeStatus != "none" {
		setWorkspacePRMergeStatusForTest(t, prMergeStatus)
	}
	return c
}

func (c autoClose) watches(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM issue_wakeup WHERE issue_id = $1 AND system_rule = 'workflow_edges'`, c.issueID)
}

func (c autoClose) setPRState(t *testing.T, prID, state string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE github_pull_request SET state = $2 WHERE id = $1`, prID, state)
}

func (c autoClose) wantPending(t *testing.T) {
	t.Helper()
	if status, _, _ := issueStatusAndAssignee(t, c.issueID); status != "agent_accepted" {
		t.Fatalf("status = %q, want agent_accepted", status)
	}
	if n := c.acceptorRuns(t); n != 0 {
		t.Fatalf("acceptor runs = %d, want 0 while the merge is pending", n)
	}
	if n := c.watches(t); n != 1 {
		t.Fatalf("own watchers = %d, want 1", n)
	}
	if n := c.activities(t, "auto_close_pending_merge"); n != 1 {
		t.Fatalf("auto_close_pending_merge rows = %d, want 1", n)
	}
}

// A head-bound PASS with the PR still open: no run starts, the issue's own
// watcher waits, and the merge then closes it with no extra task.
func TestCloseOrDispatch_DelayedMergeAfterHeadBoundPassClosesWithZeroExtraRuns(t *testing.T) {
	for name, prMergeStatus := range map[string]string{"edge evaluator moves": "none", "bridge decides": "done"} {
		t.Run(name, func(t *testing.T) {
			c := newCloseOrDispatch(t, "block", prMergeStatus)
			c.seedVerdict(t, "review_passed", "[]", autoCloseHead, "1 hour")
			prID := c.newPR(t, autoCloseHead)
			c.enter(t)
			c.wantPending(t)
			tickEdges(t)
			c.wantPending(t)
			c.setPRState(t, prID, "merged")
			dbfx.Exec(t, `UPDATE github_pull_request SET base_ref = 'main', default_branch = 'main' WHERE id = $1`, prID)
			dbfx.Exec(t, `UPDATE issue_wakeup SET next_fire_at = now() WHERE issue_id = $1 AND system_rule = 'workflow_edges'`, c.issueID)
			tickEdges(t)
			if status, _, _ := issueStatusAndAssignee(t, c.issueID); status != "done" {
				t.Fatalf("status after the merge = %q, want done", status)
			}
			if n := c.acceptorRuns(t); n != 0 {
				t.Fatalf("acceptor runs = %d, want 0", n)
			}
		})
	}
}

func TestCloseOrDispatch_MergeAlreadyStoredAtEntryClosesWithTheWatcherPresent(t *testing.T) {
	c := newCloseOrDispatch(t, "block", "none")
	c.seedVerdict(t, "review_passed", "[]", autoCloseHead, "1 hour")
	c.mergedPR(t, autoCloseHead, "main", "main")
	c.enter(t)
	c.wantClosed(t)
}

func TestCloseOrDispatch_NoPRClosesWithTheWatcherPresent(t *testing.T) {
	c := newCloseOrDispatch(t, "block", "none")
	c.seedVerdict(t, "review_passed", "[]", "", "1 hour")
	c.enter(t)
	c.wantClosed(t)
}

// Every negative of the step 2 table dispatches the handler run with the
// watcher present: a separate live wakeup, a stale or voided PASS, and the rest.
func TestCloseOrDispatch_EachFailedPredicateDispatchesWithTheWatcherPresent(t *testing.T) {
	for name, tc := range autoCloseNegatives {
		t.Run(name, func(t *testing.T) {
			c := newCloseOrDispatch(t, "block", "none")
			c.seedVerdict(t, "review_passed", "[]", "", "1 hour")
			tc.setup(t, c)
			c.enter(t)
			c.wantHandlerRun(t)
		})
	}
}

func TestCloseOrDispatch_MissingPassDispatches(t *testing.T) {
	c := newCloseOrDispatch(t, "block", "none")
	c.newPR(t, autoCloseHead)
	c.enter(t)
	c.wantHandlerRun(t)
}

// Only an open PR at the head the PASS names waits for the merge.
func TestCloseOrDispatch_OnlyAnOpenPRAtThePassHeadWaits(t *testing.T) {
	for name, tc := range map[string]struct{ prHead, state string }{
		"open at another head":   {autoCloseHead2, "open"},
		"closed without merge":   {autoCloseHead, "closed"},
		"merged at another head": {autoCloseHead2, "merged"},
	} {
		t.Run(name, func(t *testing.T) {
			c := newCloseOrDispatch(t, "block", "none")
			c.seedVerdict(t, "review_passed", "[]", autoCloseHead, "1 hour")
			c.setPRState(t, c.newPR(t, tc.prHead), tc.state)
			c.enter(t)
			c.wantHandlerRun(t)
		})
	}
}

// Without its own watcher nothing would ever deliver the merge, so an open PR dispatches.
func TestCloseOrDispatch_NoWatcherMeansNoPendingMerge(t *testing.T) {
	c := newAutoClose(t, "block") // agent_accepted has only an agent_done edge: no watcher
	c.seedVerdict(t, "review_passed", "[]", autoCloseHead, "1 hour")
	c.newPR(t, autoCloseHead)
	c.enter(t)
	c.wantHandlerRun(t)
}

// warn and off keep today's behavior: the run starts and no pending row is written.
func TestCloseOrDispatch_WarnAndOffStartTheRunAsToday(t *testing.T) {
	for _, mode := range []string{"warn", "off"} {
		t.Run(mode, func(t *testing.T) {
			c := newCloseOrDispatch(t, mode, "none")
			c.seedVerdict(t, "review_passed", "[]", autoCloseHead, "1 hour")
			c.newPR(t, autoCloseHead)
			c.enter(t)
			c.wantHandlerRun(t)
			if n := c.activities(t, "auto_close_pending_merge"); n != 0 {
				t.Fatalf("auto_close_pending_merge rows = %d, want 0", n)
			}
		})
	}
}

// The edge transaction: an edge that enters merged runs the same check before
// it inserts the handler's run.
var closeOrDispatchEdgeSteps = []map[string]any{
	{"status_key": "in_progress"},
	{"status_key": "agent_review", "edges": []map[string]any{
		{"to": "in_progress", "guard": "agent_verdict(CHANGES)"},
		{"to": "in_review", "guard": "agent_verdict(PASS)"},
	}},
	{"status_key": "in_review", "edges": []map[string]any{
		{"to": "merged", "guard": "pr.merged", "timeout": "72h", "on_timeout": "blocked"},
	}},
	{"status_key": "merged", "edges": []map[string]any{{"to": "done", "guard": "agent_done"}}},
	{"status_key": "blocked"},
	{"status_key": "done"},
}

func newCloseOrDispatchEdgeEntry(t *testing.T) autoClose {
	t.Helper()
	setWorkspacePRMergeStatusForTest(t, "none")
	c := newAutoClose(t, "block")
	createTestCustomStatus(t, "merged", "started")
	wf := createTestWorkflowSteps(t, "Close or dispatch edge "+workflowTestSuffix(), closeOrDispatchEdgeSteps)
	setProjectWorkflow(t, c.projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	setColumnHandoff(t, c.projectID, "merged", handToAgent(c.acceptor, "Close it.")).Want(http.StatusOK)
	dbfx.Exec(t, `UPDATE issue SET status = 'in_review', assignee_type = NULL, assignee_id = NULL WHERE id = $1`, c.issueID)
	c.step = "merged"
	return c
}

func TestCloseOrDispatch_EdgeTransactionClosesBeforeEnqueue(t *testing.T) {
	c := newCloseOrDispatchEdgeEntry(t)
	c.seedVerdict(t, "review_passed", "[]", autoCloseHead, "1 hour")
	c.mergedPR(t, autoCloseHead, "main", "main")
	tickEdges(t)
	c.wantClosed(t)
}

func TestCloseOrDispatch_EdgeTransactionDispatchesOnAStalePass(t *testing.T) {
	c := newCloseOrDispatchEdgeEntry(t)
	c.seedVerdict(t, "review_passed", "[]", autoCloseHead, "1 hour")
	c.mergedPR(t, autoCloseHead2, "main", "main")
	tickEdges(t)
	c.wantHandlerRun(t)
}
