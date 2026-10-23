package handler

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
)

// CHE-1966: a green head wakes the owner (pr_checks_passed) and, in a project
// whose step asks for it, moves the issue one workflow step.

type greenSnapshot struct {
	mu      sync.Mutex
	current ghsnapshot.PRSnapshot
}

func newGreenSnapshot(head string) *greenSnapshot {
	return &greenSnapshot{current: ghsnapshot.PRSnapshot{HeadSHA: head, BaseRef: "main", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN", RollupState: "SUCCESS", HasChecks: true}}
}

func (g *greenSnapshot) get() ghsnapshot.PRSnapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.current
}

// set moves the PR to a new head with the given rollup, as a push would.
func (g *greenSnapshot) set(t *testing.T, prNumber int32, head, rollup string) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.current.HeadSHA, g.current.RollupState = head, rollup
	if _, err := testPool.Exec(context.Background(), `UPDATE github_pull_request SET head_sha=$2 WHERE workspace_id=$1 AND pr_number=$3`, testWorkspaceID, head, prNumber); err != nil {
		t.Fatal(err)
	}
}

func TestPRChecksPassedWakesOwnerOncePerHead(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 1966001
	const prNumber int32 = 196601
	fx := newPRMergeWebhookFixture(t, installationID)
	agentID := createHandlerTestAgent(t, "CHE-1966 green wake", nil)
	setIssueAssigneeDirect(t, fx.child.ID, "agent", agentID)
	linkedPRForWakeup(t, fx, installationID, prNumber, "che1966-head-a")
	setWorkspacePRWakeSetting(t, "github_enabled", true)
	snap := newGreenSnapshot("che1966-head-a")
	refresh := startSnapshotRefresh(t, installationID, prNumber, snap.get)
	count := func() int { return prRuleWakeups(t, fx.child.ID, agentID, "pr_checks_passed") }
	finish := func() {
		testPool.Exec(context.Background(), `UPDATE agent_task_queue SET status='completed',completed_at=clock_timestamp() WHERE issue_id=$1 AND status IN ('queued','dispatched','running')`, fx.child.ID)
	}

	refresh()
	if got := count(); got != 0 {
		t.Fatalf("green head with the rule off by default queued %d wakeups, want 0", got)
	}

	setWorkspacePRWakeSetting(t, "github_wake_on_ci_success", true)
	refresh()
	if got := count(); got != 1 {
		t.Fatalf("green head queued %d wakeups, want 1", got)
	}
	finish()
	refresh()
	if got := count(); got != 1 {
		t.Fatalf("same green head woke again: %d wakeups, want 1", got)
	}

	snap.set(t, prNumber, "che1966-head-b", "FAILURE")
	refresh()
	if got := count(); got != 1 {
		t.Fatalf("red head queued a green wakeup: %d, want 1", got)
	}

	finish() // the red head's own wakeup run would otherwise carry the next one
	snap.set(t, prNumber, "che1966-head-c", "SUCCESS")
	refresh()
	if got := count(); got != 2 {
		t.Fatalf("new green head queued %d wakeups in total, want 2", got)
	}
}

func TestCIGreenAdvancesWorkflowExactlyOneStep(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	seedTestCatalog(t)
	const installationID int64 = 1966002
	const prNumber int32 = 196602
	fx := newPRMergeWebhookFixture(t, installationID)
	reviewer := seededReadyAgentID(t)
	// in_review is flagged too and loops back to in_progress, so a second
	// advance on the same head would show as a status flip.
	wf := createTestWorkflowSteps(t, "CI advance "+workflowTestSuffix(), []map[string]any{
		{"status_key": "backlog"},
		{"status_key": "in_progress", "advance_on_ci_green": true},
		{"status_key": "in_review", "advance_on_ci_green": true, "next_status_key": "in_progress",
			"handler": map[string]any{"type": "agent", "id": reviewer}, "instructions": "Review the PR."},
		{"status_key": "blocked"},
	})
	projectID := createWorkflowTestProject(t, "CI advance project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	if _, err := testPool.Exec(context.Background(), `UPDATE issue SET project_id=$2, status='in_progress', assignee_type=NULL, assignee_id=NULL WHERE id=$1`, fx.child.ID, projectID); err != nil {
		t.Fatal(err)
	}
	linkedPRForWakeup(t, fx, installationID, prNumber, "che1966-adv-a")
	setWorkspacePRWakeSetting(t, "github_enabled", true)
	snap := newGreenSnapshot("che1966-adv-a")
	refresh := startSnapshotRefresh(t, installationID, prNumber, snap.get)
	status := func() string { s, _, _ := issueStatusAndAssignee(t, fx.child.ID); return s }

	snap.set(t, prNumber, "che1966-adv-a", "FAILURE")
	refresh()
	if got := status(); got != "in_progress" {
		t.Fatalf("red head moved the issue to %s", got)
	}

	snap.set(t, prNumber, "che1966-adv-a", "SUCCESS")
	refresh()
	got, assigneeType, assigneeID := issueStatusAndAssignee(t, fx.child.ID)
	if got != "in_review" || assigneeType != "agent" || assigneeID != reviewer {
		t.Fatalf("green head left the issue at %s/%s/%s, want in_review handed to %s", got, assigneeType, assigneeID, reviewer)
	}
	if n := countPendingTasksForAgent(t, fx.child.ID, reviewer); n != 1 {
		t.Fatalf("handoff queued %d runs for the next step's handler, want 1", n)
	}

	refresh()
	if got := status(); got != "in_review" {
		t.Fatalf("same green head advanced again to %s", got)
	}
	// Back on a flagged step at the same head: spent, not re-armed.
	testPool.Exec(context.Background(), `UPDATE issue SET status='in_progress' WHERE id=$1`, fx.child.ID)
	refresh()
	if got := status(); got != "in_progress" {
		t.Fatalf("a head that already advanced the issue advanced it again to %s", got)
	}

	snap.set(t, prNumber, "che1966-adv-b", "SUCCESS")
	refresh()
	if got := status(); got != "in_review" {
		t.Fatalf("new green head left the issue at %s, want in_review", got)
	}
}

func TestCIGreenDoesNotAdvanceUnflaggedStep(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	seedTestCatalog(t)
	const installationID int64 = 1966003
	const prNumber int32 = 196603
	fx := newPRMergeWebhookFixture(t, installationID)
	wf := createTestWorkflowSteps(t, "CI no advance "+workflowTestSuffix(), []map[string]any{
		{"status_key": "backlog"}, {"status_key": "in_progress"}, {"status_key": "in_review"}, {"status_key": "blocked"},
	})
	projectID := createWorkflowTestProject(t, "CI no advance project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	testPool.Exec(context.Background(), `UPDATE issue SET project_id=$2, status='in_progress' WHERE id=$1`, fx.child.ID, projectID)
	linkedPRForWakeup(t, fx, installationID, prNumber, "che1966-flat-a")
	setWorkspacePRWakeSetting(t, "github_enabled", true)
	refresh := startSnapshotRefresh(t, installationID, prNumber, newGreenSnapshot("che1966-flat-a").get)
	refresh()
	if got, _, _ := issueStatusAndAssignee(t, fx.child.ID); got != "in_progress" {
		t.Fatalf("a step without the option moved to %s", got)
	}
}
