package handler

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// wantRemapRow checks the one status_changed row a workflow remap wrote.
func wantRemapRow(t *testing.T, issueID, from, to string) {
	t.Helper()
	rows := statusRows(t, issueID)
	if len(rows) != 1 {
		t.Fatalf("status_changed rows = %d, want 1: %v", len(rows), rows)
	}
	row := rows[0]
	trigger, _ := row["trigger"].(map[string]any)
	cause, _ := trigger["cause"].(map[string]any)
	by, _ := cause["by"].(map[string]any)
	if row["from"] != from || row["to"] != to {
		t.Fatalf("row moves %v -> %v, want %s -> %s", row["from"], row["to"], from, to)
	}
	if cause["kind"] != "workflow_remap" || by["type"] != "member" || by["id"] != testUserID {
		t.Fatalf("cause = %v, want workflow_remap by member %s", cause, testUserID)
	}
	if trigger["auth_source"] != nil {
		t.Fatalf("auth_source = %v, want null", trigger["auth_source"])
	}
	// A remap is not a person's move, so it opens no run-budget window.
	if _, err := testHandler.Queries.LatestMemberStatusChange(context.Background(), parseUUID(issueID)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("remap row opened a budget window (LatestMemberStatusChange err = %v)", err)
	}
}

// Switching a project's workflow writes one history row per remapped issue.
func TestProjectWorkflowSwitchRecordsRemapHistory(t *testing.T) {
	seedTestCatalog(t)
	resetDefaultWorkflow(t)
	projectID := createWorkflowTestProject(t, "Workflow remap history switch project")
	issueID := dbfx.Issue(t, "remapped by switch", testutil.Cols{"project_id": projectID, "status": "in_review"})
	untouched := dbfx.Issue(t, "not remapped", testutil.Cols{"project_id": projectID, "status": "todo"})

	wf := createTestWorkflow(t, "Remap history switch "+workflowTestSuffix())
	setProjectWorkflow(t, projectID, map[string]any{
		"workflow_id": wf.ID, "status_mapping": map[string]string{"in_review": "done"},
	}).Want(http.StatusOK)

	wantRemapRow(t, issueID, "in_review", "done")
	if rows := statusRows(t, untouched); len(rows) != 0 {
		t.Fatalf("an issue the switch did not move got %d history rows", len(rows))
	}
}

// Editing a workflow so it drops a status writes one history row per
// remapped issue.
func TestWorkflowEditRecordsRemapHistory(t *testing.T) {
	seedTestCatalog(t)
	resetDefaultWorkflow(t)
	full := createTestWorkflow(t, "Remap history edit "+workflowTestSuffix(), "in_progress")
	projectID := createWorkflowTestProject(t, "Workflow remap history edit project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": full.ID}).Want(http.StatusOK)
	issueID := dbfx.Issue(t, "remapped by edit", testutil.Cols{"project_id": projectID, "status": "blocked"})

	testutil.Call(t, testHandler.UpdateIssueWorkflow, withURLParam(workflowRequest("PATCH", "/api/issue-workflows/"+full.ID, map[string]any{
		"steps": statusSteps("backlog", "in_progress"), "status_mapping": map[string]string{"blocked": "in_progress"},
	}), "id", full.ID)).Want(http.StatusOK)

	wantRemapRow(t, issueID, "blocked", "in_progress")
}
