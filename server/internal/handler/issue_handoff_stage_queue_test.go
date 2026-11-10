package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-4906: a handoff into a review step whose reviewer already holds its WIP
// limit of review runs creates no run (the stage queue refuses it and the
// scheduler starts it later). The timeline row must say so, never run=true.

// reviewerFull gives the ready agent `n` review-stage PRs that each hold a
// running run, older than any PR created in the test.
func reviewerFull(t *testing.T, agentID string, n int) {
	t.Helper()
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID)
	for i := 0; i < n; i++ {
		issueID := dbfx.Issue(t, "stage slot holder", testutil.Cols{
			"status": "agent_review", "assignee_type": "agent", "assignee_id": agentID,
		})
		linkOpenPR(t, issueID, "now() - interval '2 hours'")
		dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running"})
	}
}

func linkOpenPR(t *testing.T, issueID, createdAt string) {
	t.Helper()
	prID := dbfx.Insert(t, "github_pull_request", testutil.Cols{
		"workspace_id": testWorkspaceID, "installation_id": 1, "repo_owner": "multica-ai", "repo_name": "multica",
		"pr_number": testutil.Raw("(floor(random() * 1000000000))::int"), "title": "stage PR", "state": "open",
		"html_url": "https://example.test/pr", "pr_created_at": testutil.Raw(createdAt), "pr_updated_at": testutil.Raw("now()"),
	})
	dbfx.Exec(t, `INSERT INTO issue_pull_request (issue_id, pull_request_id) VALUES ($1, $2)`, issueID, prID)
	t.Cleanup(func() { testPool.Exec(t.Context(), `DELETE FROM issue_pull_request WHERE pull_request_id = $1`, prID) })
}

func reviewHandoffIssue(t *testing.T, agentID string) string {
	t.Helper()
	createTestCustomStatus(t, "agent_review", "started")
	wf := createTestWorkflowSteps(t, "Stage queue handoff "+workflowTestSuffix(), []map[string]any{
		{"status_key": "in_progress"},
		{"status_key": "agent_review"},
		{"status_key": "in_review"},
		{"status_key": "blocked"},
	})
	projectID := createWorkflowTestProject(t, "Stage queue handoff project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	setColumnHandoff(t, projectID, "agent_review", handToAgent(agentID, "Review it.")).Want(http.StatusOK)
	issueID := dbfx.Issue(t, "stage queue handoff", testutil.Cols{"project_id": projectID, "status": "in_progress"})
	linkOpenPR(t, issueID, "now()")
	return issueID
}

func taskCount(t *testing.T, issueID string) (n int) {
	t.Helper()
	dbfx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID).Scan(&n)
	return n
}

func TestHandoffIntoFullReviewStageRecordsNoRun(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	reviewerFull(t, agentID, 2)
	issueID := reviewHandoffIssue(t, agentID)
	ids := recordActivityIDs(t, issueID)

	testutil.Call(t, testHandler.MoveIssue, moveStatus(issueID, "agent_review")).Want(http.StatusOK)

	if n := taskCount(t, issueID); n != 0 {
		t.Fatalf("tasks on the issue = %d, want 0: the stage queue refuses the run", n)
	}
	row := wantOneRecordedRow(t, issueID, "ui_or_api", ids())
	if row["handoff_run"] == "true" {
		t.Fatalf("handoff_run = true although no run exists; details = %v", row)
	}
	if row["handoff_run_reason"] != "stage_queue_full" {
		t.Fatalf("handoff_run_reason = %v, want stage_queue_full; details = %v", row["handoff_run_reason"], row)
	}
}

// With a free slot the same handoff starts its run and says so.
func TestHandoffIntoReviewStageWithFreeSlotRecordsRun(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	reviewerFull(t, agentID, 1)
	issueID := reviewHandoffIssue(t, agentID)
	ids := recordActivityIDs(t, issueID)

	testutil.Call(t, testHandler.MoveIssue, moveStatus(issueID, "agent_review")).Want(http.StatusOK)

	if n := taskCount(t, issueID); n != 1 {
		t.Fatalf("tasks on the issue = %d, want 1", n)
	}
	row := wantOneRecordedRow(t, issueID, "ui_or_api", ids())
	if row["handoff_run"] != "true" || row["handoff_run_reason"] == "stage_queue_full" {
		t.Fatalf("handoff details = %v, want run=true", row)
	}
}
