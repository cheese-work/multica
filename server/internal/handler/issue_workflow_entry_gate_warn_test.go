package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// The response to a write that posts the first-warning comment carries the
// revision after that comment, so the caller's next edit does not conflict
// with the platform's own comment (CHE-2531 fix round).
func TestEntryGateWarnResponseCarriesCurrentRevision(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	installationID := gateInstall + 800
	c := newEdgeCase(t, installationID, gateSteps(gate("pr.ready", "warn")))
	linkGatePRs(t, c, installationID, draftPR)
	c.enter(t, "in_progress")
	var moved struct {
		Revision int64 `json:"revision"`
	}
	putIssue(t, c.issueID, map[string]any{"status": "in_review"}).Want(http.StatusOK).JSON(&moved)
	var stored int64
	dbfx.QueryRow(t, `SELECT revision FROM issue WHERE id=$1`, c.issueID).Scan(&stored)
	if moved.Revision != stored {
		t.Fatalf("response revision = %d, stored revision = %d", moved.Revision, stored)
	}
	putIssue(t, c.issueID, map[string]any{"title": "next edit", "expected_revision": moved.Revision}).Want(http.StatusOK)
}

// A warn gate that fails at create is recorded like one that fails on a move:
// one activity row and the step's first comment, committed with the issue, and
// the created issue carries the revision after that comment.
func TestEntryGateWarnOnCreate(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	seedTestCatalog(t)
	steps := []map[string]any{
		{"status_key": "backlog"},
		{"status_key": "in_review", "entry": []map[string]any{gate("pr.ready", "warn")}},
	}
	wf := createTestWorkflowSteps(t, "Create warn "+workflowTestSuffix(), steps)
	projectID := createWorkflowTestProject(t, "Create warn project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	body := map[string]any{"title": "warned create", "project_id": projectID, "status": "in_review"}
	var created struct {
		ID       string `json:"id"`
		Revision int64  `json:"revision"`
	}
	testutil.Call(t, testHandler.CreateIssue, newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, body)).Want(http.StatusCreated).JSON(&created)
	if n := activityCount(t, created.ID, "entry_gate_warning"); n != 1 {
		t.Fatalf("%d warning activity rows, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id=$1 AND content LIKE 'Entry gate warning%'`, created.ID); n != 1 {
		t.Fatalf("%d warning comments, want 1", n)
	}
	var stored int64
	dbfx.QueryRow(t, `SELECT revision FROM issue WHERE id=$1`, created.ID).Scan(&stored)
	if created.Revision != stored {
		t.Fatalf("created revision = %d, stored revision = %d", created.Revision, stored)
	}
}

// The warning key is the real head of every in-scope PR, not the first PR's
// checks-snapshot head: a second PR, or a push whose checks snapshot has not
// arrived, is a new warning.
func TestEntryGateWarnKeysOnEveryRealHead(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	installationID := gateInstall + 1000
	c := newEdgeCase(t, installationID, gateSteps(gate("pr.ready", "warn")))
	linkGatePRs(t, c, installationID, draftPR)
	enterReview := func() {
		t.Helper()
		c.enter(t, "in_progress")
		putIssue(t, c.issueID, map[string]any{"status": "in_review"}).Want(http.StatusOK)
	}
	rows := func() int { return activityCount(t, c.issueID, "entry_gate_warning") }
	enterReview()
	enterReview()
	if got := rows(); got != 1 {
		t.Fatalf("one PR, same head twice: %d warning rows, want 1", got)
	}
	// A push: the PR has a new head, the checks snapshot still belongs to the old one.
	dbfx.Exec(t, `UPDATE github_pull_request SET head_sha='gate-pushed-head' WHERE installation_id=$1`, installationID)
	enterReview()
	if got := rows(); got != 2 {
		t.Fatalf("push awaiting its checks snapshot: %d warning rows, want 2", got)
	}
	// A second PR on the issue's child, with its own head.
	linkGatePRs(t, c, installationID+1, draftPR)
	enterReview()
	if got := rows(); got != 3 {
		t.Fatalf("second PR: %d warning rows, want 3", got)
	}
	enterReview()
	if got := rows(); got != 3 {
		t.Fatalf("same heads again: %d warning rows, want 3", got)
	}
}
