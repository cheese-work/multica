package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5242: a workflow with include_child_prs counts the direct children's PRs
// in the PR merge rule, with the edge's rule (one merged, none open or draft, a
// closed-unmerged PR does not block, children status ignored). Without it the
// issue's own links decide and every child must be done or cancelled. A child
// PR event and a child reaching a terminal status re-run the parent's decision.

var prChildSteps = append(append([]map[string]any{}, prHandoffSteps...), map[string]any{"status_key": "done"})

type prChildCase struct {
	parentID, parentIdentifier string
	childIDs, childIdentifiers [2]string
	agentID                    string
	secret                     string
}

// newPRChildCase is a parent sitting in_review with two in_progress children,
// all in one project whose workflow lists the merged step. include sets the
// workflow's include_child_prs; handler hands the merged step to an agent.
func newPRChildCase(t *testing.T, installationID int64, include, handler bool) prChildCase {
	t.Helper()
	c := prChildCase{secret: "che5242-secret"}
	t.Setenv("GITHUB_WEBHOOK_SECRET", c.secret)
	seedTestCatalog(t)
	createTestCustomStatus(t, "merged", "started")
	c.agentID = seededReadyAgentID(t)
	wf := createTestWorkflowSteps(t, "PR children "+workflowTestSuffix(), prChildSteps)
	if handler {
		setWorkflowHandoff(t, wf.ID, "merged", handToAgent(c.agentID, "Verify landing.")).Want(http.StatusOK)
	}
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `UPDATE issue_workflow SET include_child_prs=$2 WHERE id=$1`, wf.ID, include); err != nil {
		t.Fatal(err)
	}
	projectID := createWorkflowTestProject(t, "PR children project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	parent := prAutoCompleteTestIssue(t, "pr children parent", installationID)
	c.parentID, c.parentIdentifier = parent.ID, parent.Identifier
	if _, err := testPool.Exec(ctx,
		`UPDATE issue SET project_id=$2, status='in_review', assignee_type=NULL, assignee_id=NULL WHERE id=$1`, parent.ID, projectID); err != nil {
		t.Fatal(err)
	}
	for i := range c.childIDs {
		child := createChildIssue(t, "pr child", "in_progress", parent.ID)
		c.childIDs[i], c.childIdentifiers[i] = child.ID, child.Identifier
		t.Cleanup(func() {
			bg := context.Background()
			testPool.Exec(bg, `DELETE FROM issue_pull_request WHERE issue_id = $1`, child.ID)
			testPool.Exec(bg, `DELETE FROM activity_log WHERE issue_id = $1`, child.ID)
			testPool.Exec(bg, `DELETE FROM agent_task_queue WHERE issue_id = $1`, child.ID)
			testPool.Exec(bg, `DELETE FROM issue WHERE id = $1`, child.ID)
		})
		if _, err := testPool.Exec(ctx, `UPDATE issue SET project_id=$2 WHERE id=$1`, child.ID, projectID); err != nil {
			t.Fatal(err)
		}
	}
	setWorkspacePRMergeStatusForTest(t, "merged")
	return c
}

// pr fires a webhook for a PR titled with child i's identifier.
func (c prChildCase) pr(t *testing.T, installationID int64, i int, number int32, lifecycle string) {
	t.Helper()
	firePRWebhook(t, c.secret, installationID, number, c.childIdentifiers[i]+": work", "", "feat/che5242-"+c.childIdentifiers[i], lifecycle)
}

func (c prChildCase) parentStatus(t *testing.T) string { return issueStatusForTest(t, c.parentID) }

func (c prChildCase) childDone(t *testing.T, i int) {
	t.Helper()
	req := withURLParam(newRequest("PATCH", "/api/issues/"+c.childIDs[i], map[string]any{"status": "done"}), "id", c.childIDs[i])
	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)
}

func TestPRMergeIncludeChildPRsMovesParentOnLastChildMerge(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	const installationID int64 = 5242001
	c := newPRChildCase(t, installationID, true, true)

	c.pr(t, installationID, 0, 11, "opened")
	c.pr(t, installationID, 1, 12, "opened")
	c.pr(t, installationID, 0, 11, "merged")
	if got := c.parentStatus(t); got != "in_review" {
		t.Fatalf("parent = %q with child PR #12 still open, want in_review", got)
	}
	c.pr(t, installationID, 1, 12, "merged")

	wantHandedOff(t, c.parentID, c.agentID)
	if n := len(statusRows(t, c.parentID)); n != 1 {
		t.Fatalf("parent status_changed rows = %d, want 1", n)
	}
}

// The edge's rule: a closed-unmerged PR does not block beside a merged one, and
// the children's own status is ignored (both children are still in a started
// status or moved to merged, never done).
func TestPRMergeIncludeChildPRsClosedUnmergedDoesNotBlock(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	const installationID int64 = 5242002
	c := newPRChildCase(t, installationID, true, false)

	c.pr(t, installationID, 0, 21, "opened")
	c.pr(t, installationID, 1, 22, "opened")
	c.pr(t, installationID, 0, 21, "closed")
	c.pr(t, installationID, 1, 22, "merged")

	if got := c.parentStatus(t); got != "merged" {
		t.Fatalf("parent = %q, want merged: one merged child PR and none open", got)
	}
}

// Without include_child_prs the issue's own PRs decide, and every child must be
// done or cancelled. The last child reaching done moves the parent with no new
// GitHub event.
func TestPRMergeOwnPRsWaitForChildrenThenMoveOnChildTerminal(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	const installationID int64 = 5242003
	c := newPRChildCase(t, installationID, false, false)

	firePRWebhook(t, c.secret, installationID, 31, c.parentIdentifier+": work", "", "feat/che5242-own", "merged")
	if got := c.parentStatus(t); got != "in_review" {
		t.Fatalf("parent = %q with open children, want in_review", got)
	}
	c.childDone(t, 0)
	if got := c.parentStatus(t); got != "in_review" {
		t.Fatalf("parent = %q with one child left, want in_review", got)
	}
	c.childDone(t, 1)
	if got := c.parentStatus(t); got != "merged" {
		t.Fatalf("parent = %q after the last child is done, want merged", got)
	}
	if n := len(statusRows(t, c.parentID)); n != 1 {
		t.Fatalf("parent status_changed rows = %d, want 1", n)
	}
}

// With include_child_prs a child reaching a terminal status re-runs the
// parent's decision. The PRs are merged in the database without an event, so
// only the terminal status can trigger the move.
func TestPRMergeIncludeChildPRsRerunsOnChildTerminal(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	const installationID int64 = 5242004
	c := newPRChildCase(t, installationID, true, false)

	c.pr(t, installationID, 0, 41, "opened")
	if _, err := testPool.Exec(context.Background(),
		`UPDATE github_pull_request SET state='merged' WHERE id IN (SELECT pull_request_id FROM issue_pull_request WHERE issue_id=$1)`, c.childIDs[0]); err != nil {
		t.Fatal(err)
	}
	if got := c.parentStatus(t); got != "in_review" {
		t.Fatalf("parent = %q before any event, want in_review", got)
	}
	c.childDone(t, 0)

	if got := c.parentStatus(t); got != "merged" {
		t.Fatalf("parent = %q after a child reached done, want merged", got)
	}
	if n := len(statusRows(t, c.parentID)); n != 1 {
		t.Fatalf("parent status_changed rows = %d, want 1", n)
	}
}
