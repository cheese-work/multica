package handler

import (
	"net/http"
	"testing"
)

// The gate binds the status a write ends on, not the status it names: a
// project move can change it too (CHE-2531 fix round).

// gatedProjectMove is an issue in a loose project at status, and the id of a
// project whose workflow has only backlog and a gated in_review.
func gatedProjectMove(t *testing.T, installationID int64, status string) (c edgeCase, gatedProject string) {
	t.Helper()
	c = newEdgeCase(t, installationID, []map[string]any{
		{"status_key": "backlog"},
		{"status_key": "in_review", "entry": []map[string]any{gate("pr.ready", "block")}},
	})
	dbfx.QueryRow(t, `SELECT project_id::text FROM issue WHERE id=$1`, c.issueID).Scan(&gatedProject)
	loose := createWorkflowTestProject(t, "Loose project")
	dbfx.Exec(t, `UPDATE issue SET project_id=$2, status=$3 WHERE id=$1`, c.issueID, loose, status)
	return c, gatedProject
}

func TestEntryGateBindsProjectMoveRemap(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for name, enter := range map[string]bool{"plain move": false, "enter_workflow_step": true} {
		t.Run(name, func(t *testing.T) {
			installationID := gateInstall + 700
			if enter {
				installationID++
			}
			// in_progress is not a step of the gated workflow; the move remaps
			// it to in_review, the first step of its category.
			c, gated := gatedProjectMove(t, installationID, "in_progress")
			body := map[string]any{"project_id": gated}
			if enter {
				body["enter_workflow_step"] = true
			}
			res := putIssue(t, c.issueID, body)
			res.Want(http.StatusConflict)
			if got := failedGuards(t, res); len(got) != 1 || got[0] != "pr.ready" {
				t.Fatalf("failed guards = %v, want [pr.ready]; body %s", got, res.Text())
			}
			wantIssueStatus(t, c.issueID, "in_progress")
		})
	}
}

// A project move that keeps the status is no entry into a step.
func TestEntryGateLeavesProjectMoveThatKeepsStatus(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c, gated := gatedProjectMove(t, gateInstall+702, "in_review")
	putIssue(t, c.issueID, map[string]any{"project_id": gated}).Want(http.StatusOK)
	wantIssueStatus(t, c.issueID, "in_review")
}
