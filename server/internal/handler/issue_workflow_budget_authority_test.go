package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// Workflow budget fields change only with an interactive session (CHE-2829,
// CHE-2798 S1b). Every other credential is refused 403 when the write changes
// the cap; a write that leaves it unchanged keeps today's authorization.
func TestWorkflowBudgetFieldsNeedInteractiveSession(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	wf := createTestWorkflowSteps(t, "Budget authority "+workflowTestSuffix(), statusSteps("backlog", "in_progress", "blocked"))
	put := func(source string, body map[string]any) *testutil.Response {
		req := withURLParam(workflowRequest("PUT", "/api/issue-workflows/"+wf.ID, body), "id", wf.ID)
		if source != "" {
			req.Header.Set("X-Actor-Source", source)
		}
		return testutil.Call(t, testHandler.UpdateIssueWorkflow, req)
	}
	storedCap := func() *int {
		t.Helper()
		var out IssueWorkflowResponse
		req := withURLParam(workflowRequest("GET", "/api/issue-workflows/"+wf.ID, nil), "id", wf.ID)
		testutil.Call(t, testHandler.GetIssueWorkflow, req).Want(http.StatusOK).JSON(&out)
		return out.MaxRunsPerIssue
	}

	for _, source := range []string{"pat", "cli_jwt", "cloud_pat", "task_token"} {
		put(source, map[string]any{"max_runs_per_issue": 9}).Want(http.StatusForbidden)
		if got := storedCap(); got != nil {
			t.Fatalf("%s: refused write stored cap %d", source, *got)
		}
	}

	put("interactive_jwt", map[string]any{"max_runs_per_issue": 7}).Want(http.StatusOK)
	if got := storedCap(); got == nil || *got != 7 {
		t.Fatalf("interactive write stored %v, want 7", got)
	}

	for _, source := range []string{"pat", "cli_jwt", "cloud_pat", "task_token"} {
		put(source, map[string]any{"max_runs_per_issue": 0}).Want(http.StatusForbidden)
		put(source, map[string]any{"max_runs_per_issue": 8}).Want(http.StatusForbidden)
		if got := storedCap(); got == nil || *got != 7 {
			t.Fatalf("%s: refused write moved cap to %v, want 7", source, got)
		}
	}

	// Unchanged budget fields keep today's authorization.
	put("pat", map[string]any{"max_runs_per_issue": 7, "description": "same cap"}).Want(http.StatusOK)
	put("cli_jwt", map[string]any{"description": "no budget field"}).Want(http.StatusOK)
	put("", map[string]any{"include_child_prs": false}).Want(http.StatusOK)
}

// A decision-bound task-token step write still cannot carry a cap change: the
// cap has its own workflow.budget decision (CHE-5228), so a mixed body is
// malformed and the refusal leaves both the cap and the steps as stored.
func TestBoundWorkflowPatchCannotChangeRunCap(t *testing.T) {
	edit := setInstructions("agent text")
	write := newBoundWorkflowWrite(t, "workflow.instructions", edit, "workflow.instructions")
	body := write.body(t, write.steps(t, edit))
	body["max_runs_per_issue"] = 9

	write.patch(t, body).Want(http.StatusBadRequest)

	if got := write.storedInstructions(t, "in_progress"); got == "agent text" {
		t.Fatal("refused write changed the steps")
	}
	var stored *int32
	dbfx.QueryRow(t, `SELECT max_runs_per_issue FROM issue_workflow WHERE id = $1`, write.workflowID).Scan(&stored)
	if stored != nil {
		t.Fatalf("refused write stored cap %d", *stored)
	}
}
