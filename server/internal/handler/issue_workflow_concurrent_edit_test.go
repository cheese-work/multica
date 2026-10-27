package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func patchWorkflow(t *testing.T, id string, body map[string]any) *testutil.Response {
	t.Helper()
	return testutil.Call(t, testHandler.UpdateIssueWorkflow, withURLParam(workflowRequest("PATCH", "/api/issue-workflows/"+id, body), "id", id))
}

// workflowVersion is the version token a workflow response carries. updated_at
// has one-second resolution, which two quick saves can share, so the token is
// the timestamp at full precision.
func workflowVersion(t *testing.T, resp *testutil.Response) string {
	t.Helper()
	var body struct {
		Version string `json:"version"`
	}
	resp.JSON(&body)
	if body.Version == "" {
		t.Fatalf("the workflow response carries no version: %s", resp.Body.String())
	}
	return body.Version
}

// Two admins edit one workflow. The server serializes the writes, but a whole
// workflow payload replaces what the other admin saved first unless the save
// names the version it edited. A save that names a version other than the
// current one is refused, and the stored workflow stays as the other admin
// left it. The two saves here land within one second on purpose.
func TestUpdateIssueWorkflowRefusesAStaleExpectedVersion(t *testing.T) {
	seedTestCatalog(t)
	wf := createTestWorkflow(t, "Concurrent edit "+workflowTestSuffix(), "in_progress")
	edited := workflowVersion(t, testutil.Call(t, testHandler.GetIssueWorkflow, withURLParam(workflowRequest("GET", "/api/issue-workflows/"+wf.ID, nil), "id", wf.ID)).Want(http.StatusOK))

	newer := workflowVersion(t, patchWorkflow(t, wf.ID, map[string]any{"description": "saved by the other admin", "expected_version": edited}).Want(http.StatusOK))
	if newer == edited {
		t.Fatalf("the first save did not move the version: %s", newer)
	}

	resp := patchWorkflow(t, wf.ID, map[string]any{"description": "stale whole-workflow payload", "expected_version": edited})
	resp.Want(http.StatusConflict)
	var body struct {
		Code string `json:"code"`
	}
	resp.JSON(&body)
	if body.Code != "workflow_changed_elsewhere" {
		t.Fatalf("code = %q, want workflow_changed_elsewhere", body.Code)
	}

	var stored string
	dbfx.QueryRow(t, `SELECT description FROM issue_workflow WHERE id = $1`, wf.ID).Scan(&stored)
	if stored != "saved by the other admin" {
		t.Fatalf("stored description = %q: the stale save overwrote the newer one", stored)
	}
}

func TestUpdateIssueWorkflowAcceptsTheCurrentExpectedVersion(t *testing.T) {
	seedTestCatalog(t)
	wf := createTestWorkflow(t, "Current version "+workflowTestSuffix(), "in_progress")
	v0 := workflowVersion(t, testutil.Call(t, testHandler.GetIssueWorkflow, withURLParam(workflowRequest("GET", "/api/issue-workflows/"+wf.ID, nil), "id", wf.ID)).Want(http.StatusOK))

	v1 := workflowVersion(t, patchWorkflow(t, wf.ID, map[string]any{"description": "first", "expected_version": v0}).Want(http.StatusOK))
	patchWorkflow(t, wf.ID, map[string]any{"description": "second", "expected_version": v1}).Want(http.StatusOK)
}

// Clients that predate the field send no version and keep last-writer-wins.
func TestUpdateIssueWorkflowWithoutAnExpectedVersionStillWrites(t *testing.T) {
	seedTestCatalog(t)
	wf := createTestWorkflow(t, "No version "+workflowTestSuffix(), "in_progress")
	patchWorkflow(t, wf.ID, map[string]any{"description": "one"}).Want(http.StatusOK)
	patchWorkflow(t, wf.ID, map[string]any{"description": "two"}).Want(http.StatusOK)
}

// A caller who may not edit the workflow is refused with 403 even when the
// version they name is stale; the 409 would tell them the workflow changed
// without letting them act on it.
func TestUpdateIssueWorkflowChecksEditRightsBeforeTheVersion(t *testing.T) {
	seedTestCatalog(t)
	wf := createTestWorkflow(t, "Rights first "+workflowTestSuffix(), "in_progress")
	_, asOther := addWorkflowTestMember(t, "other")

	req := asOther(workflowRequest("PATCH", "/api/issue-workflows/"+wf.ID, map[string]any{"description": "x", "expected_version": "stale"}))
	testutil.Call(t, testHandler.UpdateIssueWorkflow, withURLParam(req, "id", wf.ID)).Want(http.StatusForbidden)
}
