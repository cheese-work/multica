package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func waitsFlag(t *testing.T, resp *testutil.Response, statusKey string) bool {
	t.Helper()
	var wf IssueWorkflowResponse
	resp.JSON(&wf)
	for _, s := range wf.Steps {
		if s.StatusKey == statusKey {
			return s.WaitsOnOpenDecision != nil && *s.WaitsOnOpenDecision
		}
	}
	t.Fatalf("status %q not in workflow response", statusKey)
	return false
}

func getWorkflow(t *testing.T, id string) *testutil.Response {
	t.Helper()
	return testutil.Call(t, testHandler.GetIssueWorkflow, withURLParam(workflowRequest("GET", "/api/issue-workflows/"+id, nil), "id", id)).Want(http.StatusOK)
}

// CHE-4976: the step flag waits_on_open_decision round-trips through the
// documented workflow API, and an edit that omits it keeps the stored value.
func TestWorkflowStepWaitsOnOpenDecisionRoundTrip(t *testing.T) {
	seedTestCatalog(t)
	steps := statusSteps("backlog", "in_progress", "in_review", "blocked")
	steps[2]["waits_on_open_decision"] = true
	wf := createTestWorkflowSteps(t, "Waits "+workflowTestSuffix(), steps)

	if !waitsFlag(t, getWorkflow(t, wf.ID), "in_review") {
		t.Fatal("waits_on_open_decision not returned after create")
	}

	// A steps replacement that omits the field keeps the stored value.
	patchWorkflow(t, wf.ID, map[string]any{"steps": statusSteps("backlog", "in_progress", "in_review", "blocked")}).Want(http.StatusOK)
	if !waitsFlag(t, getWorkflow(t, wf.ID), "in_review") {
		t.Fatal("steps replacement without the field dropped waits_on_open_decision")
	}

	// A handoff edit that omits the field keeps it.
	setWorkflowHandoff(t, wf.ID, "in_review", map[string]any{"instructions": "edited"}).Want(http.StatusOK)
	if !waitsFlag(t, getWorkflow(t, wf.ID), "in_review") {
		t.Fatal("handoff edit without the field dropped waits_on_open_decision")
	}

	// Explicit false clears it, through both write paths.
	off := statusSteps("backlog", "in_progress", "in_review", "blocked")
	off[2]["waits_on_open_decision"] = false
	patchWorkflow(t, wf.ID, map[string]any{"steps": off}).Want(http.StatusOK)
	if waitsFlag(t, getWorkflow(t, wf.ID), "in_review") {
		t.Fatal("explicit false did not clear waits_on_open_decision")
	}
	setWorkflowHandoff(t, wf.ID, "in_review", map[string]any{"waits_on_open_decision": true}).Want(http.StatusOK)
	if !waitsFlag(t, getWorkflow(t, wf.ID), "in_review") {
		t.Fatal("handoff write did not set waits_on_open_decision")
	}
}
