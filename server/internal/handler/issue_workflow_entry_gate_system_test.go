package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// System writers move an issue without a person or run at the keyboard, so an
// entry gate never holds them. Each test puts a failing block gate on the step
// the writer enters and checks the move still lands (CHE-2531 fix round).

// blockedStepProject is a project whose workflow gates every step the
// writers enter behind a block gate no issue can pass (no linked PR).
func blockedStepProject(t *testing.T, statuses ...string) string {
	t.Helper()
	seedTestCatalog(t)
	steps := []map[string]any{{"status_key": "backlog"}, {"status_key": "todo"}}
	for _, s := range statuses {
		steps = append(steps, map[string]any{"status_key": s, "entry": []map[string]any{gate("pr.ready", "block")}})
	}
	wf := createTestWorkflowSteps(t, "System writer "+workflowTestSuffix(), steps)
	projectID := createWorkflowTestProject(t, "System writer project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	return projectID
}

func TestEntryGateSkipsGitHubMergeCompletion(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	projectID := blockedStepProject(t, "done")
	issueID := dbfx.Issue(t, "gated merge", testutil.Cols{"project_id": projectID, "status": "in_progress"})
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	if err != nil {
		t.Fatal(err)
	}

	testHandler.advanceIssueToDone(context.Background(), issue, testWorkspaceID)

	wantIssueStatus(t, issueID, "done")
}
