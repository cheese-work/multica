package handler

import (
	"net/http"
	"slices"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// A batch status change used to ignore `handoff_preview`, so the issues the
// client believed would not hand off could still be handed to a workflow
// handler it never confirmed. A batch binds one result for every issue: that
// none of them hands off.
func TestBatchStatusRefusesAHandoffTheClientDidNotPreview(t *testing.T) {
	seedTestCatalog(t)
	handler := seededReadyAgentID(t)
	negative := map[string]any{
		"previous_assignee_type": nil, "previous_assignee_id": nil,
		"handler_type": nil, "handler_id": nil,
	}
	batch := func(ids []string, updates map[string]any) *testutil.Response {
		return testutil.Call(t, testHandler.BatchUpdateIssues, newRequest("POST", "/api/issues/batch?workspace_id="+testWorkspaceID, map[string]any{
			"issue_ids": ids,
			"updates":   updates,
		}))
	}
	type batchResult struct {
		Updated    int      `json:"updated"`
		UpdatedIDs []string `json:"updated_ids"`
	}

	t.Run("an issue that would hand off is skipped and the rest are written", func(t *testing.T) {
		handingOff := dbfx.Issue(t, "batch handing off", testutil.Cols{
			"project_id": deliveryProject(t, "Batch binding project "+workflowTestSuffix(), handler), "status": "todo",
		})
		plain := dbfx.Issue(t, "batch plain", testutil.Cols{"status": "todo"})

		var got batchResult
		batch([]string{handingOff, plain}, map[string]any{"status": "in_progress", "handoff_preview": negative}).
			Want(http.StatusOK).JSON(&got)

		wantIssueStatus(t, handingOff, "todo")
		if _, _, assignee := issueStatusAndAssignee(t, handingOff); assignee != "" {
			t.Fatalf("assignee = %q, want none: nobody confirmed the handoff", assignee)
		}
		wantIssueStatus(t, plain, "in_progress")
		if got.Updated != 1 || !slices.Equal(got.UpdatedIDs, []string{plain}) {
			t.Fatalf("updated = %d %v, want 1 [%s]: the response names the issues it wrote", got.Updated, got.UpdatedIDs, plain)
		}
	})

	t.Run("an issue whose step hands off to no one is written", func(t *testing.T) {
		projectID := createWorkflowTestProject(t, "Batch binding plain project "+workflowTestSuffix())
		issueID := dbfx.Issue(t, "batch binding plain", testutil.Cols{"project_id": projectID, "status": "todo"})

		var got batchResult
		batch([]string{issueID}, map[string]any{"status": "in_review", "handoff_preview": negative}).
			Want(http.StatusOK).JSON(&got)

		wantIssueStatus(t, issueID, "in_review")
		if !slices.Equal(got.UpdatedIDs, []string{issueID}) {
			t.Fatalf("updated_ids = %v, want [%s]", got.UpdatedIDs, issueID)
		}
	})

	t.Run("a batch cannot carry a handler binding", func(t *testing.T) {
		issueID := dbfx.Issue(t, "batch positive binding", testutil.Cols{
			"project_id": deliveryProject(t, "Batch positive project "+workflowTestSuffix(), handler), "status": "todo",
		})
		batch([]string{issueID}, map[string]any{
			"status": "in_progress",
			"handoff_preview": map[string]any{
				"previous_assignee_type": nil, "previous_assignee_id": nil,
				"handler_type": "agent", "handler_id": handler,
			},
		}).Want(http.StatusBadRequest)
		wantIssueStatus(t, issueID, "todo")
	})

	t.Run("a batch without a binding behaves as before", func(t *testing.T) {
		issueID := dbfx.Issue(t, "batch legacy", testutil.Cols{
			"project_id": deliveryProject(t, "Batch legacy project "+workflowTestSuffix(), handler), "status": "todo",
		})
		batch([]string{issueID}, map[string]any{"status": "in_progress"}).Want(http.StatusOK)
		if status, _, assignee := issueStatusAndAssignee(t, issueID); status != "in_progress" || assignee != handler {
			t.Fatalf("status/assignee = %s/%s, want in_progress handed to %s", status, assignee, handler)
		}
	})
}
