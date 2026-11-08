package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-3113: a workflow's start status is an unstarted or started step of the
// workflow; anything else is refused and the stored value stays.
func TestUpdateIssueWorkflowStartStatus(t *testing.T) {
	seedTestCatalog(t)
	wf := createTestWorkflow(t, "Start status "+workflowTestSuffix(), "in_progress")

	var got IssueWorkflowResponse
	patchWorkflow(t, wf.ID, map[string]any{"start_status_key": "in_progress"}).Want(http.StatusOK).JSON(&got)
	if got.StartStatusKey != "in_progress" {
		t.Fatalf("start_status_key = %q, want in_progress", got.StartStatusKey)
	}

	for _, key := range []string{"done", "cancelled", "code_review_not_in_workflow"} {
		patchWorkflow(t, wf.ID, map[string]any{"start_status_key": key}).Want(http.StatusBadRequest)
	}
	var stored string
	dbfx.QueryRow(t, `SELECT COALESCE(start_status_key, '') FROM issue_workflow WHERE id = $1`, wf.ID).Scan(&stored)
	if stored != "in_progress" {
		t.Fatalf("stored start status = %q after refused writes, want in_progress", stored)
	}

	// A write that leaves the field out keeps it; "" restores todo.
	patchWorkflow(t, wf.ID, map[string]any{"description": "unrelated"}).Want(http.StatusOK).JSON(&got)
	if got.StartStatusKey != "in_progress" {
		t.Fatalf("start_status_key = %q after an unrelated write, want in_progress", got.StartStatusKey)
	}
	patchWorkflow(t, wf.ID, map[string]any{"start_status_key": ""}).Want(http.StatusOK).JSON(&got)
	if got.StartStatusKey != "" {
		t.Fatalf("start_status_key = %q after clearing, want empty", got.StartStatusKey)
	}
}

// An HTTP create with no status starts the issue on the workflow's start status.
func TestCreateIssueUsesWorkflowStartStatus(t *testing.T) {
	seedTestCatalog(t)
	wf := createTestWorkflow(t, "Start create "+workflowTestSuffix(), "in_progress")
	patchWorkflow(t, wf.ID, map[string]any{"start_status_key": "in_progress"}).Want(http.StatusOK)
	projectID := createWorkflowTestProject(t, "start project "+workflowTestSuffix())
	dbfx.Exec(t, `UPDATE project SET workflow_id = $1 WHERE id = $2`, wf.ID, projectID)

	var created IssueResponse
	testutil.Call(t, testHandler.CreateIssue, newRequest("POST", "/api/issues", map[string]any{
		"title": "start status " + workflowTestSuffix(), "project_id": projectID,
	})).Want(http.StatusCreated).JSON(&created)
	t.Cleanup(func() { testPool.Exec(t.Context(), `DELETE FROM issue WHERE id = $1`, created.ID) })
	if created.Status != "in_progress" {
		t.Fatalf("status = %q, want in_progress", created.Status)
	}
}
