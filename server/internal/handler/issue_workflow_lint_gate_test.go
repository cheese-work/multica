package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// Save-time lint gate (CHE-3658 layer 4).

func leadStep(key string) map[string]any {
	return map[string]any{"status_key": key, "handler": map[string]any{"type": "project_lead"}}
}

// cleanSteps has a handler on every non-backlog status: no lint error.
func cleanSteps() []map[string]any {
	return []map[string]any{
		{"status_key": "backlog"}, leadStep("todo"), leadStep("in_progress"), leadStep("blocked"),
	}
}

// brokenSteps leaves in_progress without a handler or an exit: WF002 and WF003.
func brokenSteps() []map[string]any {
	return []map[string]any{
		{"status_key": "backlog"}, leadStep("todo"), {"status_key": "in_progress"}, leadStep("blocked"),
	}
}

func saveWorkflow(t *testing.T, id string, body map[string]any) *testutil.Response {
	t.Helper()
	return testutil.Call(t, testHandler.UpdateIssueWorkflow, withURLParam(workflowRequest("PUT", "/api/issue-workflows/"+id, body), "id", id))
}

func storedOverride(t *testing.T, id string) (reason *string, at *string) {
	t.Helper()
	if err := testPool.QueryRow(context.Background(),
		`SELECT lint_override_reason, lint_overridden_at::text FROM issue_workflow WHERE id = $1`, id).Scan(&reason, &at); err != nil {
		t.Fatalf("read stored override: %v", err)
	}
	return reason, at
}

func TestWorkflowSaveRefusedOnLintError(t *testing.T) {
	seedTestCatalog(t)
	wf := createTestWorkflowSteps(t, "Gate refuse "+workflowTestSuffix(), cleanSteps())

	var body struct {
		Code     string `json:"code"`
		Error    string `json:"error"`
		Findings []struct {
			Rule string `json:"rule"`
			Hint string `json:"hint"`
		} `json:"findings"`
	}
	saveWorkflow(t, wf.ID, map[string]any{"steps": brokenSteps()}).Want(http.StatusUnprocessableEntity).JSON(&body)
	if body.Code != "workflow_lint_errors" {
		t.Fatalf("code = %q, want workflow_lint_errors", body.Code)
	}
	rules := map[string]bool{}
	for _, f := range body.Findings {
		rules[f.Rule] = true
		if f.Hint == "" {
			t.Fatalf("finding %s has no fix hint", f.Rule)
		}
	}
	if !rules["WF002"] || !rules["WF003"] {
		t.Fatalf("want WF002 and WF003 in the refusal, got %+v", body.Findings)
	}
	// Nothing was written.
	got := workflowVersion(t, testutil.Call(t, testHandler.GetIssueWorkflow, withURLParam(workflowRequest("GET", "/api/issue-workflows/"+wf.ID, nil), "id", wf.ID)).Want(http.StatusOK))
	if got != wf.Version {
		t.Fatalf("refused save changed the workflow: %s -> %s", wf.Version, got)
	}
}

func TestWorkflowSaveAcceptedWithOverrideStoresReason(t *testing.T) {
	seedTestCatalog(t)
	wf := createTestWorkflowSteps(t, "Gate override "+workflowTestSuffix(), cleanSteps())

	saveWorkflow(t, wf.ID, map[string]any{"steps": brokenSteps(), "lint_override": "Gate step is assigned at run time; CHE-3655"}).Want(http.StatusOK)
	reason, at := storedOverride(t, wf.ID)
	if reason == nil || *reason != "Gate step is assigned at run time; CHE-3655" || at == nil {
		t.Fatalf("override not stored: reason=%v at=%v", reason, at)
	}

	// A later save that passes clean clears the record.
	saveWorkflow(t, wf.ID, map[string]any{"steps": cleanSteps()}).Want(http.StatusOK)
	if reason, at = storedOverride(t, wf.ID); reason != nil || at != nil {
		t.Fatalf("clean save kept the old override: reason=%v at=%v", reason, at)
	}
}

func TestWorkflowSaveOverrideNeedsAReason(t *testing.T) {
	seedTestCatalog(t)
	wf := createTestWorkflowSteps(t, "Gate blank "+workflowTestSuffix(), cleanSteps())
	saveWorkflow(t, wf.ID, map[string]any{"steps": brokenSteps(), "lint_override": "   "}).Want(http.StatusBadRequest)
}

func TestWorkflowSaveWithOnlyWarningsReturnsThem(t *testing.T) {
	seedTestCatalog(t)
	wf := createTestWorkflowSteps(t, "Gate warn "+workflowTestSuffix(), brokenSteps()[:1])

	var out IssueWorkflowResponse
	saveWorkflow(t, wf.ID, map[string]any{"steps": cleanSteps()}).Want(http.StatusOK).JSON(&out)
	found := false
	for _, f := range out.LintFindings {
		if f.Level == "error" && !f.Waived {
			t.Fatalf("a clean save returned an error finding: %+v", f)
		}
		if f.Rule == "WF014" && f.Level == "warning" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want the WF014 warning in the response, got %+v", out.LintFindings)
	}
	if reason, _ := storedOverride(t, wf.ID); reason != nil {
		t.Fatalf("no override was given, but one is stored: %v", *reason)
	}
}

func TestWorkflowSaveWaivedErrorDoesNotBlock(t *testing.T) {
	seedTestCatalog(t)
	wf := createTestWorkflowSteps(t, "Gate waive "+workflowTestSuffix(), cleanSteps())
	steps := brokenSteps()
	steps[2]["lint_waive"] = []map[string]any{
		{"rule": "WF002", "reason": "assigned at run time"},
		{"rule": "WF003", "reason": "assigned at run time"},
	}
	var out IssueWorkflowResponse
	saveWorkflow(t, wf.ID, map[string]any{"steps": steps}).Want(http.StatusOK).JSON(&out)
	// The waiver survives the save and shows in the response.
	waived := false
	for _, s := range out.Steps {
		if s.StatusKey == "in_progress" && len(s.LintWaive) == 2 {
			waived = true
		}
	}
	if !waived {
		t.Fatalf("lint_waive was dropped by the save: %+v", out.Steps)
	}
}

func TestWorkflowDescriptionEditIsNotGated(t *testing.T) {
	seedTestCatalog(t)
	wf := createTestWorkflowSteps(t, "Gate desc "+workflowTestSuffix(), cleanSteps())
	saveWorkflow(t, wf.ID, map[string]any{"steps": brokenSteps(), "lint_override": "known gap"}).Want(http.StatusOK)
	// The stored steps still carry errors; a rename-only edit must not be refused.
	saveWorkflow(t, wf.ID, map[string]any{"description": "still editable"}).Want(http.StatusOK)
}
