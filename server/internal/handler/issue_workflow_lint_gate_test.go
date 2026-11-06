package handler

import (
	"context"
	"net/http"
	"strings"
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

// A rename or description edit saves the workflow as it stands, so it is
// linted too: it cannot slip an invalid workflow past the gate.
func TestWorkflowDescriptionEditIsGated(t *testing.T) {
	seedTestCatalog(t)
	wf := createTestWorkflowSteps(t, "Gate desc "+workflowTestSuffix(), cleanSteps())
	saveWorkflow(t, wf.ID, map[string]any{"steps": brokenSteps(), "lint_override": "known gap"}).Want(http.StatusOK)

	saveWorkflow(t, wf.ID, map[string]any{"description": "still broken"}).Want(http.StatusUnprocessableEntity)
	saveWorkflow(t, wf.ID, map[string]any{"description": "still broken", "lint_override": "rename only"}).Want(http.StatusOK)
}

// The audit record outlives the next save: each override stays in the log with
// the version it let through, even after a clean save clears the row.
func TestWorkflowLintOverrideLogKeepsEveryReason(t *testing.T) {
	seedTestCatalog(t)
	wf := createTestWorkflowSteps(t, "Gate log "+workflowTestSuffix(), cleanSteps())

	v1 := workflowVersion(t, saveWorkflow(t, wf.ID, map[string]any{"steps": brokenSteps(), "lint_override": "first reason"}).Want(http.StatusOK))
	saveWorkflow(t, wf.ID, map[string]any{"steps": cleanSteps()}).Want(http.StatusOK)
	saveWorkflow(t, wf.ID, map[string]any{"steps": brokenSteps(), "lint_override": "second reason"}).Want(http.StatusOK)
	saveWorkflow(t, wf.ID, map[string]any{"steps": cleanSteps()}).Want(http.StatusOK)

	rows, err := testPool.Query(context.Background(),
		`SELECT reason, to_char(version AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US') FROM issue_workflow_lint_override WHERE workflow_id = $1 ORDER BY version`, wf.ID)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	defer rows.Close()
	var reasons, versions []string
	for rows.Next() {
		var reason, version string
		if err := rows.Scan(&reason, &version); err != nil {
			t.Fatal(err)
		}
		reasons, versions = append(reasons, reason), append(versions, version)
	}
	if len(reasons) != 2 || reasons[0] != "first reason" || reasons[1] != "second reason" {
		t.Fatalf("log = %v, want both reasons in order", reasons)
	}
	if !strings.HasPrefix(v1, versions[0]) {
		t.Fatalf("first log row is for version %s, want the saved version %s", versions[0], v1)
	}
}

// Warnings are stored with the workflow, so a later read still shows them.
func TestWorkflowLintFindingsAreReadBack(t *testing.T) {
	seedTestCatalog(t)
	wf := createTestWorkflowSteps(t, "Gate readback "+workflowTestSuffix(), brokenSteps()[:1])
	saveWorkflow(t, wf.ID, map[string]any{"steps": cleanSteps()}).Want(http.StatusOK)

	var out IssueWorkflowResponse
	testutil.Call(t, testHandler.GetIssueWorkflow, withURLParam(workflowRequest("GET", "/api/issue-workflows/"+wf.ID, nil), "id", wf.ID)).Want(http.StatusOK).JSON(&out)
	for _, f := range out.LintFindings {
		if f.Rule == "WF014" && f.Level == "warning" {
			return
		}
	}
	t.Fatalf("want the stored WF014 warning on a plain read, got %+v", out.LintFindings)
}

// The findings saved with a workflow are read back by every workspace member.
// A finding derived from an agent or its runtime must not be stored, or an
// admin's save would hand a private runtime's provider and version to a member
// who may not see that runtime.
func TestWorkflowStoredFindingsLeaveOutRuntimeDetails(t *testing.T) {
	seedTestCatalog(t)
	agentID := createHandlerTestAgent(t, "Gate runtime leak "+workflowTestSuffix(), nil)
	wf := createTestWorkflowSteps(t, "Gate runtime leak "+workflowTestSuffix(), brokenSteps()[:1])
	steps := cleanSteps()
	steps[2] = map[string]any{"status_key": "in_progress", "handler": map[string]any{"type": "agent", "id": agentID}}

	// The runtime of the test agent is not write-qualified: WF005 for the saver.
	saveWorkflow(t, wf.ID, map[string]any{"steps": steps, "lint_override": "test"}).Want(http.StatusOK)

	var raw string
	if err := testPool.QueryRow(context.Background(), `SELECT lint_findings::text FROM issue_workflow WHERE id = $1`, wf.ID).Scan(&raw); err != nil {
		t.Fatalf("read stored findings: %v", err)
	}
	var out IssueWorkflowResponse
	testutil.Call(t, testHandler.GetIssueWorkflow, withURLParam(workflowRequest("GET", "/api/issue-workflows/"+wf.ID, nil), "id", wf.ID)).Want(http.StatusOK).JSON(&out)
	for _, f := range out.LintFindings {
		if f.Rule == "WF005" || strings.Contains(f.Message, "handler_test_runtime") {
			t.Fatalf("a plain read returns runtime details: %+v", f)
		}
	}
	if strings.Contains(raw, "handler_test_runtime") {
		t.Fatalf("the stored findings keep the runtime provider: %s", raw)
	}
}

// The saver sees every finding in the save response, agent-derived ones
// included; only the stored readback leaves them out.
func TestWorkflowSaveResponseKeepsAgentFindings(t *testing.T) {
	seedTestCatalog(t)
	agentID := createHandlerTestAgent(t, "Gate response "+workflowTestSuffix(), nil)
	wf := createTestWorkflowSteps(t, "Gate response "+workflowTestSuffix(), brokenSteps()[:1])
	steps := cleanSteps()
	steps[2] = map[string]any{"status_key": "in_progress", "handler": map[string]any{"type": "agent", "id": agentID}}

	var out IssueWorkflowResponse
	saveWorkflow(t, wf.ID, map[string]any{"steps": steps, "lint_override": "test"}).Want(http.StatusOK).JSON(&out)
	for _, f := range out.LintFindings {
		if f.Rule == "WF005" {
			return
		}
	}
	t.Fatalf("want the WF005 finding in the save response, got %+v", out.LintFindings)
}
