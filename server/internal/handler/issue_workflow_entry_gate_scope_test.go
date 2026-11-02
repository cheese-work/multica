package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// What entry gates bind and what they leave alone (CHE-2531).

// Only a write that changes the status into the step is gated: a title edit,
// or the same status again, on an issue already there passes.
func TestEntryGateLeavesOtherWritesOnAGatedStep(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	installationID := gateInstall + 500
	c := newEdgeCase(t, installationID, gateSteps(gate("pr.checks_green", "block")))
	linkGatePRs(t, c, installationID, openRed)
	c.enter(t, "in_review")
	putIssue(t, c.issueID, map[string]any{"title": "renamed while CI is red"}).Want(http.StatusOK)
	putIssue(t, c.issueID, map[string]any{"status": "in_review"}).Want(http.StatusOK)
	putIssue(t, c.issueID, map[string]any{"status": "in_progress"}).Want(http.StatusOK)
	putIssue(t, c.issueID, map[string]any{"status": "in_review"}).Want(http.StatusConflict)
}

// Edge moves (and with them CI-green and run-budget moves, which share
// moveIssueOnEdge) are system moves and skip entry gates.
func TestEntryGateDoesNotBindEdgeMoves(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	installationID := gateInstall + 600
	steps := append([]map[string]any(nil), mergeEdgeSteps...)
	steps[3] = map[string]any{"status_key": "in_review", "entry": []map[string]any{gate("pr.checks_green", "block")}}
	c := newEdgeCase(t, installationID, steps)
	linkGatePRs(t, c, installationID, mergedPR)
	c.enter(t, "in_progress")
	tickEdges(t)
	wantIssueStatus(t, c.issueID, "in_review")
}

// A workflow edit that remaps issues out of a removed status moves them into a
// gated step without the gate.
func TestEntryGateDoesNotBindWorkflowRemap(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	seedTestCatalog(t)
	wf := createTestWorkflowSteps(t, "Remap gate "+workflowTestSuffix(), gateSteps(gate("pr.ready", "block")))
	projectID := createWorkflowTestProject(t, "Remap gate project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	stuck := dbfx.Issue(t, "stuck", testutil.Cols{"project_id": projectID, "status": "blocked"})
	req := withURLParam(workflowRequest("PATCH", "/api/issue-workflows/"+wf.ID, map[string]any{
		"steps":          statusSteps("backlog", "todo", "in_progress", "in_review"),
		"status_mapping": map[string]string{"blocked": "in_review"},
	}), "id", wf.ID)
	var out IssueWorkflowResponse
	testutil.Call(t, testHandler.UpdateIssueWorkflow, req).Want(http.StatusOK).JSON(&out)
	wantIssueStatus(t, stuck, "in_review")
	for _, s := range out.Steps {
		if s.StatusKey == "in_review" && len(s.Entry) != 1 {
			t.Fatalf("remap edit dropped the stored gate: %+v", s.Entry)
		}
	}
}

// A save that omits `entry` (an older client) keeps the stored gates; an
// explicit empty list clears them.
func TestEntryGatesSurviveAnOlderClientSave(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	wf := createTestWorkflowSteps(t, "Preserve gate "+workflowTestSuffix(), gateSteps(gate("pr.ready", "block")))
	save := func(steps []map[string]any) IssueWorkflowResponse {
		t.Helper()
		var out IssueWorkflowResponse
		req := withURLParam(workflowRequest("PATCH", "/api/issue-workflows/"+wf.ID, map[string]any{"steps": steps}), "id", wf.ID)
		testutil.Call(t, testHandler.UpdateIssueWorkflow, req).Want(http.StatusOK).JSON(&out)
		return out
	}
	entryOf := func(w IssueWorkflowResponse) int {
		for _, s := range w.Steps {
			if s.StatusKey == "in_review" {
				return len(s.Entry)
			}
		}
		return -1
	}
	if got := entryOf(save(statusSteps("backlog", "todo", "in_progress", "in_review", "blocked"))); got != 1 {
		t.Fatalf("a save without entry left %d gates, want the stored 1", got)
	}
	cleared := gateSteps([]map[string]any{}...) // a nil slice would encode as null, which reads as omitted
	cleared[3]["entry"] = []map[string]any{}
	if got := entryOf(save(cleared)); got != 0 {
		t.Fatalf("an explicit empty entry left %d gates, want 0", got)
	}
}

// Writes that never go through a workflow write are not gated: PR auto-complete
// moves the issue to done on a merged PR.
func TestEntryGateDoesNotBindPRAutoComplete(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	secret := "entry-gate-auto-complete-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	const installationID int64 = 2531700
	seedTestCatalog(t)
	steps := gateSteps()
	steps = append(steps, map[string]any{"status_key": "done", "entry": []map[string]any{gate("pr.ready", "block")}})
	wf := createTestWorkflowSteps(t, "Auto-complete gate "+workflowTestSuffix(), steps)
	projectID := createWorkflowTestProject(t, "Auto-complete gate project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	issue := prAutoCompleteTestIssue(t, "gated done", installationID)
	dbfx.Exec(t, `UPDATE issue SET project_id=$2 WHERE id=$1`, issue.ID, projectID)
	firePRWebhook(t, secret, installationID, 1, issue.Identifier+": delivery", "", "fix/gated-done", "merged")
	if got := issueStatusForTest(t, issue.ID); got != "done" {
		t.Fatalf("status = %q, want done: a merge completes the issue past a pr.ready gate on done", got)
	}
}
