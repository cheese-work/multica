package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// Workflow lint API (CHE-3658).

func lintOne(t *testing.T, workflowID string) WorkflowLintResponse {
	t.Helper()
	req := withURLParam(workflowRequest("GET", "/api/issue-workflows/"+workflowID+"/lint", nil), "id", workflowID)
	var out WorkflowLintResponse
	testutil.Call(t, testHandler.LintIssueWorkflow, req).Want(http.StatusOK).JSON(&out)
	return out
}

func lintRule(r WorkflowLintResponse, rule, step string) bool {
	for _, f := range r.Findings {
		if f.Rule == rule && f.Step == step {
			return true
		}
	}
	return false
}

func TestLintIssueWorkflowReadsInstructionsAgainstCatalog(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	// The workflow has no in_review step, so "In review" is a catalog status it does not define.
	wf := createTestWorkflow(t, "Lint instructions "+workflowTestSuffix(), "in_progress")
	setWorkflowHandoff(t, wf.ID, "in_progress", handToAgent(agentID, "Build it, then move this issue to In review.")).Want(http.StatusOK)

	got := lintOne(t, wf.ID)
	if got.WorkflowID != wf.ID || !lintRule(got, "WF009", "in_progress") {
		t.Fatalf("want WF009@in_progress for %s, got %+v", wf.ID, got)
	}
	if got.Warnings == 0 {
		t.Fatalf("WF009 is a warning and must be counted: %+v", got)
	}
}

func TestLintIssueWorkflowsCoversTheWorkspace(t *testing.T) {
	seedTestCatalog(t)
	wf := createTestWorkflow(t, "Lint all "+workflowTestSuffix(), "in_progress")
	req := workflowRequest("GET", "/api/issue-workflows/lint", nil)
	var out struct {
		Workflows []WorkflowLintResponse `json:"workflows"`
		Total     int                    `json:"total"`
	}
	testutil.Call(t, testHandler.LintIssueWorkflows, req).Want(http.StatusOK).JSON(&out)
	if out.Total != len(out.Workflows) {
		t.Fatalf("total %d != %d workflows", out.Total, len(out.Workflows))
	}
	for _, w := range out.Workflows {
		if w.WorkflowID == wf.ID {
			if w.Findings == nil {
				t.Fatal("findings must be a list, not null")
			}
			return
		}
	}
	t.Fatalf("workflow %s missing from lint-all", wf.ID)
}

func TestLintIssueWorkflowUnknownIDIs404(t *testing.T) {
	id := "00000000-0000-0000-0000-000000000001"
	req := withURLParam(workflowRequest("GET", "/api/issue-workflows/"+id+"/lint", nil), "id", id)
	testutil.Call(t, testHandler.LintIssueWorkflow, req).Want(http.StatusNotFound)
}
