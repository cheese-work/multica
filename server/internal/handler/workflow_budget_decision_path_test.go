package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5228: a non-interactive credential cannot write a workflow's budget
// fields directly. The refusal names the decision path, and the child-PR
// switch is gated like the cap.
func TestWorkflowBudgetWriteWithoutDecisionPointsAtTheDecisionPath(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	wf := createTestWorkflowSteps(t, "Budget decision path "+workflowTestSuffix(), statusSteps("backlog", "in_progress", "blocked"))
	put := func(source string, body map[string]any) *testutil.Response {
		req := withURLParam(workflowRequest("PUT", "/api/issue-workflows/"+wf.ID, body), "id", wf.ID)
		req.Header.Set("X-Actor-Source", source)
		return testutil.Call(t, testHandler.UpdateIssueWorkflow, req)
	}
	stored := func() (cap *int, children bool) {
		t.Helper()
		dbfx.QueryRow(t, `SELECT max_runs_per_issue, include_child_prs FROM issue_workflow WHERE id = $1`, wf.ID).Scan(&cap, &children)
		return cap, children
	}

	for _, source := range nonInteractive {
		for name, body := range map[string]map[string]any{
			"cap":      {"max_runs_per_issue": 9},
			"children": {"include_child_prs": false},
		} {
			res := put(source, body).Want(http.StatusForbidden)
			if text := res.Text(); !strings.Contains(text, "budget_requires_decision") || !strings.Contains(text, "workflow.budget") {
				t.Fatalf("%s %s: refusal must carry budget_requires_decision and name workflow.budget: %s", source, name, text)
			}
		}
		if cap, children := stored(); cap != nil || !children {
			t.Fatalf("%s: refused writes stored cap %v children %v (want nil/true)", source, cap, children)
		}
	}

	put("interactive_jwt", map[string]any{"max_runs_per_issue": 7, "include_child_prs": false}).Want(http.StatusOK)
	if cap, children := stored(); cap == nil || *cap != 7 || children {
		t.Fatalf("interactive write stored cap %v children %v, want 7/false", cap, children)
	}
	// Unchanged values keep today's authorization.
	put("pat", map[string]any{"max_runs_per_issue": 7, "include_child_prs": false, "description": "same budget"}).Want(http.StatusOK)
}

// Create and update widen under one rule: a non-interactive credential cannot
// name a workflow whose budget is wider than the one the issue would follow.
func TestCreateIssueWorkflowWideningMatchesUpdate(t *testing.T) {
	f := newRebindFixture(t, 5228001)
	create := func(source string, projectID, workflowID string) *testutil.Response {
		body := map[string]any{"title": "CHE-5228 parity " + workflowTestSuffix(), "project_id": projectID, "workflow_id": workflowID}
		req := withActorSource(newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, body), source)
		return testutil.Call(t, testHandler.CreateIssue, req)
	}
	for _, source := range nonInteractive {
		wantRebindRefused(t, create(source, f.narrowProj, f.wide), "create widening", source)
	}
	// Narrowing and the project's own workflow stay open. task_token is left
	// out: a create it makes needs an agent id this fixture does not stamp.
	for _, source := range []string{"pat", "cloud_pat", "cli_jwt"} {
		create(source, f.wideProj, f.narrow).Want(http.StatusCreated)
		create(source, f.narrowProj, f.narrow).Want(http.StatusCreated)
	}
	create("interactive_jwt", f.narrowProj, f.wide).Want(http.StatusCreated)
}
