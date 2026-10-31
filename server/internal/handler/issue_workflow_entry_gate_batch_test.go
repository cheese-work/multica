package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// Batch, create and validation paths of entry gates (CHE-2531).

// A batch applies per issue: the issue that meets the gate moves, the one that
// does not is reported in `failed` with the gate error and stays put.
func TestEntryGateMixedBatch(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	installationID := gateInstall + 700
	c := newEdgeCase(t, installationID, gateSteps(gate("pr.ready", "block")))
	linkGatePRs(t, c, installationID, openGreen)
	var projectID string
	dbfx.QueryRow(t, `SELECT project_id::text FROM issue WHERE id=$1`, c.issueID).Scan(&projectID)
	other := dbfx.Issue(t, "no PR", testutil.Cols{"project_id": projectID, "status": "in_progress"})
	c.enter(t, "in_progress")

	req := newRequest("POST", "/api/issues/batch?workspace_id="+testWorkspaceID, map[string]any{
		"issue_ids": []string{c.issueID, other},
		"updates":   map[string]any{"status": "in_review"},
	})
	var out struct {
		Updated    int      `json:"updated"`
		UpdatedIDs []string `json:"updated_ids"`
		Failed     []struct {
			ID   string `json:"id"`
			Code string `json:"code"`
		} `json:"failed"`
	}
	testutil.Call(t, testHandler.BatchUpdateIssues, req).Want(http.StatusOK).JSON(&out)
	if out.Updated != 1 || len(out.UpdatedIDs) != 1 || out.UpdatedIDs[0] != c.issueID {
		t.Fatalf("updated = %d %v, want only %s", out.Updated, out.UpdatedIDs, c.issueID)
	}
	if len(out.Failed) != 1 || out.Failed[0].ID != other || out.Failed[0].Code != "entry_gate_failed" {
		t.Fatalf("failed = %+v, want %s with entry_gate_failed", out.Failed, other)
	}
	wantIssueStatus(t, c.issueID, "in_review")
	wantIssueStatus(t, other, "in_progress")
}

// A create is gated on the status the issue lands in, whether the caller named
// it or the workflow chose it.
func TestEntryGateOnCreateUsesEffectiveStatus(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	seedTestCatalog(t)
	steps := []map[string]any{
		{"status_key": "backlog"},
		{"status_key": "todo", "entry": []map[string]any{gate("pr.ready", "block")}},
		{"status_key": "in_review", "entry": []map[string]any{gate("pr.ready", "block")}},
		{"status_key": "blocked"},
	}
	wf := createTestWorkflowSteps(t, "Create gate "+workflowTestSuffix(), steps)
	projectID := createWorkflowTestProject(t, "Create gate project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	for name, body := range map[string]map[string]any{
		"status omitted":  {"title": "gated create", "project_id": projectID},
		"explicit status": {"title": "gated create", "project_id": projectID, "status": "in_review"},
	} {
		t.Run(name, func(t *testing.T) {
			res := testutil.Call(t, testHandler.CreateIssue, newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, body))
			res.Want(http.StatusConflict)
			if got := failedGuards(t, res); len(got) != 1 || got[0] != "pr.ready" {
				t.Fatalf("failed guards = %v, want [pr.ready]; body %s", got, res.Text())
			}
		})
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM issue WHERE project_id=$1`, projectID); n != 0 {
		t.Fatalf("a refused create left %d issues", n)
	}
	// An ungated step still creates.
	body := map[string]any{"title": "ungated create", "project_id": projectID, "status": "backlog"}
	testutil.Call(t, testHandler.CreateIssue, newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, body)).Want(http.StatusCreated)
}

func TestEntryGateValidation(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withFeatureFlag(t, testHandler, featureflags.ProjectWorkflowsV1, true)
	tooMany := make([]map[string]any, 9)
	for i := range tooMany {
		tooMany[i] = gate("pr.ready", "block")
	}
	cases := map[string][]map[string]any{
		"agent_done":    {gate("agent_done", "block")},
		"agent_verdict": {gate("agent_verdict(approved)", "block")},
		"webhook":       {gate("webhook(ci)", "block")},
		"unknown guard": {gate("pr.nope", "block")},
		"bad mode":      {gate("pr.ready", "sometimes")},
		"nine gates":    tooMany,
	}
	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			testutil.Call(t, testHandler.CreateIssueWorkflow, workflowRequest("POST", "/api/issue-workflows", map[string]any{
				"name":  "Invalid gate " + workflowTestSuffix(),
				"steps": gateSteps(entry...),
			})).Want(http.StatusBadRequest)
		})
	}
}
