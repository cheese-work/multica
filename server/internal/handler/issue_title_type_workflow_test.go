package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5271: a create without --workflow picks the workflow from the title type.
func TestCreateIssuePicksWorkflowFromTitleType(t *testing.T) {
	designWF := createTestWorkflow(t, "Design "+workflowTestSuffix())
	otherWF := createTestWorkflow(t, "Explicit "+workflowTestSuffix())
	projectID := createWorkflowTestProject(t, "Title type project")
	withFeatureFlag(t, testHandler, featureflags.ProjectWorkflowsV1, true)

	var previous []byte
	dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id=$1`, testWorkspaceID).Scan(&previous)
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings=$2::jsonb WHERE id=$1`, testWorkspaceID, string(previous))
	})
	dbfx.Exec(t, `UPDATE workspace SET settings=jsonb_set(settings,'{workflow_by_type}',$2::jsonb) WHERE id=$1`,
		testWorkspaceID, `{"design":"`+designWF.ID+`","plan":"00000000-0000-0000-0000-000000000000"}`)

	create := func(body map[string]any) IssueResponse {
		body["project_id"] = projectID
		var got IssueResponse
		testutil.Call(t, testHandler.CreateIssue, workflowRequest("POST", "/api/issues", body)).Want(http.StatusCreated).JSON(&got)
		return got
	}
	want := func(name string, got IssueResponse, id string) {
		t.Helper()
		switch {
		case id == "" && got.WorkflowID != nil:
			t.Fatalf("%s: workflow_id = %q, want project workflow (nil)", name, *got.WorkflowID)
		case id != "" && (got.WorkflowID == nil || *got.WorkflowID != id):
			t.Fatalf("%s: workflow_id = %v, want %s", name, got.WorkflowID, id)
		}
	}

	want("mapped type", create(map[string]any{"title": "design(x): new screen"}), designWF.ID)
	want("explicit wins", create(map[string]any{"title": "design(x): other", "workflow_id": otherWF.ID}), otherWF.ID)
	want("unmatched type", create(map[string]any{"title": "feat(x): thing"}), "")
	want("mapped to a missing workflow", create(map[string]any{"title": "plan(x): thing"}), "")
}
