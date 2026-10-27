package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// The handoff confirmation shows who the issue is taken from, who it goes to
// and which run is stopped. The write resolves all of that again against the
// issue as it is now. A confirmation that names what the person saw is refused
// when the issue or the workflow has moved, instead of stopping a run nobody
// previewed.
func TestHandoffConfirmationIsBoundToWhatWasPreviewed(t *testing.T) {
	seedTestCatalog(t)
	handler := seededReadyAgentID(t)
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, handler).Scan(&runtimeID)
	previewedFrom := dbfx.Agent(t, "Binding previewed "+workflowTestSuffix(), runtimeID)
	reassignedTo := dbfx.Agent(t, "Binding reassigned "+workflowTestSuffix(), runtimeID)

	binding := func(from, to string) map[string]any {
		return map[string]any{
			"previous_assignee_type": "agent", "previous_assignee_id": from,
			"handler_type": "agent", "handler_id": to,
		}
	}
	setup := func(t *testing.T, owner string) (issueID, ownerRun string) {
		projectID := deliveryProject(t, "Binding project "+workflowTestSuffix(), handler)
		issueID = dbfx.Issue(t, "binding", testutil.Cols{
			"project_id": projectID, "status": "todo", "assignee_type": "agent", "assignee_id": owner,
		})
		ownerRun = dbfx.Task(t, owner, testutil.Cols{
			"issue_id": issueID, "runtime_id": runtimeID, "status": "queued",
		})
		return issueID, ownerRun
	}

	t.Run("a preview that still matches stops the previewed agent's run", func(t *testing.T) {
		issueID, run := setup(t, previewedFrom)
		updateIssueForTest(t, issueID, map[string]any{
			"status": "in_progress", "stop_previous_assignee_runs": true,
			"handoff_preview": binding(previewedFrom, handler),
		}).Want(http.StatusOK)
		if got := taskStatus(t, run); got != "cancelled" {
			t.Fatalf("the previewed agent's run = %q, want cancelled", got)
		}
	})

	t.Run("a reassignment after the preview refuses the confirmation and stops nothing", func(t *testing.T) {
		issueID, _ := setup(t, previewedFrom)
		// Someone else hands the issue to another agent, who already has a run.
		dbfx.Exec(t, `UPDATE issue SET assignee_id = $2 WHERE id = $1`, issueID, reassignedTo)
		newOwnerRun := dbfx.Task(t, reassignedTo, testutil.Cols{"issue_id": issueID, "runtime_id": runtimeID, "status": "queued"})

		resp := updateIssueForTest(t, issueID, map[string]any{
			"status": "in_progress", "stop_previous_assignee_runs": true,
			"handoff_preview": binding(previewedFrom, handler),
		})
		resp.Want(http.StatusConflict)
		var body struct {
			Code string `json:"code"`
		}
		resp.JSON(&body)
		if body.Code != "handoff_preview_stale" {
			t.Fatalf("code = %q, want handoff_preview_stale", body.Code)
		}
		if got := taskStatus(t, newOwnerRun); got != "queued" {
			t.Fatalf("the new assignee's run = %q, want queued: the person never approved stopping it", got)
		}
		wantIssueStatus(t, issueID, "todo")
	})

	t.Run("a handler change after the preview refuses the confirmation", func(t *testing.T) {
		issueID, run := setup(t, previewedFrom)
		resp := updateIssueForTest(t, issueID, map[string]any{
			"status": "in_progress", "stop_previous_assignee_runs": true,
			// The preview named another handler than the workflow now resolves.
			"handoff_preview": binding(previewedFrom, reassignedTo),
		})
		resp.Want(http.StatusConflict)
		if got := taskStatus(t, run); got != "queued" {
			t.Fatalf("the run = %q, want queued", got)
		}
		wantIssueStatus(t, issueID, "todo")
	})

	t.Run("a write without a preview binding behaves as before", func(t *testing.T) {
		issueID, run := setup(t, previewedFrom)
		dbfx.Exec(t, `UPDATE issue SET assignee_id = $2 WHERE id = $1`, issueID, reassignedTo)
		updateIssueForTest(t, issueID, map[string]any{
			"status": "in_progress", "stop_previous_assignee_runs": true,
		}).Want(http.StatusOK)
		if got := taskStatus(t, run); got != "queued" {
			t.Fatalf("an unrelated agent's run = %q, want queued", got)
		}
	})
}
