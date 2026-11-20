package handler

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// Idle apply (CHE-7670, slice 5 of CHE-7658): a switch requested for an issue
// with no live task applies at once, the way the deferred switch applies after
// the last live task ends.

var quickStatuses = []string{"todo", "investigating", "in_progress", "agent_review", "agent_accepted", "blocked"}

// idleSwitchCase is an issue sitting on a Quick status, bound to a Quick-shaped
// workflow whose agent_accepted step waits on a merge edge, and a full workflow
// whose every step hands the issue to one agent.
type idleSwitchCase struct {
	issueID, quick, full, handler string
}

func newIdleSwitchCase(t *testing.T, status string) idleSwitchCase {
	t.Helper()
	seedTestCatalog(t)
	resetDefaultWorkflow(t)
	createTestCustomStatus(t, "investigating", "started")
	createTestCustomStatus(t, "agent_review", "started")
	createTestCustomStatus(t, "agent_accepted", "done")
	// The stored pr.merged edges need the setting off (CHE-5245).
	setWorkspacePRMergeStatusForTest(t, "none")
	handler := seededReadyAgentID(t)
	steps := func(edges map[string][]map[string]any) []map[string]any {
		out := statusSteps("backlog", "todo", "investigating", "in_progress", "agent_review", "agent_accepted", "blocked", "done", "cancelled")
		for _, step := range out {
			if e, ok := edges[step["status_key"].(string)]; ok {
				step["edges"] = e
			}
		}
		return out
	}
	quick := createTestWorkflowSteps(t, "Quick shape "+workflowTestSuffix(), steps(map[string][]map[string]any{
		"agent_accepted": {{"to": "done", "guard": "pr.merged", "timeout": "2h", "on_timeout": "agent_review"}},
	}))
	full := createTestWorkflowSteps(t, "Full shape "+workflowTestSuffix(), steps(map[string][]map[string]any{
		"in_progress": {{"to": "agent_review", "guard": "pr.merged", "timeout": "72h", "on_timeout": "blocked"}},
	}))
	for _, key := range quickStatuses {
		setWorkflowHandoff(t, full.ID, key, handToAgent(handler, "Full workflow step "+key)).Want(200)
	}
	issueID := dbfx.Issue(t, "idle switch "+status, testutil.Cols{"status": "backlog"})
	setIssueOverride(t, issueID, quick.ID)
	// A status change after the bind starts the Quick step's watch.
	dbfx.Exec(t, `UPDATE issue SET status = $2 WHERE id = $1`, issueID, status)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_workflow_switch_pending WHERE issue_id = $1`, issueID)
	})
	return idleSwitchCase{issueID: issueID, quick: quick.ID, full: full.ID, handler: handler}
}

// waitsForMerge records what Quick's agent_accepted does on entry: it waits for
// the merge with no run (CHE-7671).
func (c idleSwitchCase) waitsForMerge(t *testing.T) {
	t.Helper()
	testHandler.recordReviewActivity(context.Background(), util.MustParseUUID(c.issueID), "system", "", activityAutoClosePending, map[string]string{"predicates": "test"})
}

func (c idleSwitchCase) request(t *testing.T, workflowID string) {
	t.Helper()
	if err := testHandler.RequestWorkflowSwitch(context.Background(), util.MustParseUUID(testWorkspaceID), util.MustParseUUID(c.issueID), util.MustParseUUID(workflowID), "idle apply"); err != nil {
		t.Fatalf("request switch: %v", err)
	}
}

func (c idleSwitchCase) count(t *testing.T, query string) int {
	t.Helper()
	return dbfx.Count(t, query, c.issueID)
}

func (c idleSwitchCase) watches(t *testing.T) int {
	return c.count(t, `SELECT count(*) FROM issue_wakeup WHERE issue_id = $1 AND system_rule = 'workflow_edges'`)
}

func (c idleSwitchCase) queued(t *testing.T) int {
	return c.count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status = 'queued'`)
}

func (c idleSwitchCase) pending(t *testing.T) int {
	return c.count(t, `SELECT count(*) FROM issue_workflow_switch_pending WHERE issue_id = $1`)
}

// With no live task the switch applies inside the request: the issue keeps its
// status, follows the full workflow, belongs to that step's handler, has
// exactly one run for it, and its watcher is the full workflow's, not Quick's.
func TestIdleSwitchAppliesAtOnceOnEveryQuickStatus(t *testing.T) {
	for _, status := range quickStatuses {
		t.Run(status, func(t *testing.T) {
			c := newIdleSwitchCase(t, status)
			wantWatches := 0
			if status == "agent_accepted" {
				wantWatches = 1 // the Quick merge watch
				c.waitsForMerge(t)
			}
			if got := c.watches(t); got != wantWatches {
				t.Fatalf("before the switch: %d watches on %s, want %d", got, status, wantWatches)
			}

			c.request(t, c.full)

			wantIssueStatus(t, c.issueID, status)
			if got := issueOverride(t, c.issueID); got != c.full {
				t.Fatalf("issue follows %q, want the full workflow %q", got, c.full)
			}
			if _, at, aid := issueStatusAndAssignee(t, c.issueID); at != "agent" || aid != c.handler {
				t.Fatalf("owner = %s/%s, want the full workflow's handler %s", at, aid, c.handler)
			}
			if got := c.queued(t); got != 1 {
				t.Fatalf("queued runs = %d, want exactly one for the full workflow's step", got)
			}
			if got := c.pending(t); got != 0 {
				t.Fatalf("pending rows = %d after an idle apply", got)
			}
			// The full workflow's only fact edge is on in_progress; Quick's merge
			// watch on agent_accepted must not outlive the switch.
			wantAfter := 0
			if status == "in_progress" {
				wantAfter = 1
			}
			if got := c.watches(t); got != wantAfter {
				t.Fatalf("after the switch: %d watches on %s, want %d", got, status, wantAfter)
			}
		})
	}
}

// A live task keeps the request parked: nothing changes until its last task
// ends.
func TestIdleSwitchWithALiveTaskWaits(t *testing.T) {
	c := newIdleSwitchCase(t, "in_progress")
	dbfx.Task(t, c.handler, testutil.Cols{
		"issue_id": c.issueID, "status": "running",
		"runtime_id": testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + c.handler + "')"),
	})

	c.request(t, c.full)

	if got := issueOverride(t, c.issueID); got != c.quick {
		t.Fatalf("a live task did not stop the switch: issue follows %q", got)
	}
	if c.pending(t) != 1 || c.queued(t) != 0 {
		t.Fatalf("the request must wait: pending=%d queued=%d", c.pending(t), c.queued(t))
	}
}

// A finished issue follows the new workflow but is handed to nobody: no owner
// change, no run. agent_accepted without a pending merge is finished too.
func TestIdleSwitchOfAFinishedIssueStartsNoRun(t *testing.T) {
	for _, status := range []string{"done", "agent_accepted"} {
		t.Run(status, func(t *testing.T) {
			c := newIdleSwitchCase(t, status)

			c.request(t, c.full)

			if got := issueOverride(t, c.issueID); got != c.full {
				t.Fatalf("issue follows %q, want the full workflow %q", got, c.full)
			}
			wantIssueStatus(t, c.issueID, status)
			if _, at, _ := issueStatusAndAssignee(t, c.issueID); at != "" {
				t.Fatalf("a finished issue was handed to %s", at)
			}
			if got := c.queued(t); got != 0 {
				t.Fatalf("queued runs = %d, a finished issue starts none", got)
			}
		})
	}
}
