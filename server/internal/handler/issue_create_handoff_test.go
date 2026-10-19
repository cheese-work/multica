package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

func createdIssueAssignee(t *testing.T, issueID string) string {
	t.Helper()
	var assignee *string
	dbfx.QueryRow(t, `SELECT assignee_id::text FROM issue WHERE id = $1`, issueID).Scan(&assignee)
	if assignee == nil {
		return ""
	}
	return *assignee
}

func switchProjectWorkflow(t *testing.T, tx pgx.Tx, projectID, workflowID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM project WHERE id = $1 FOR UPDATE`, projectID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE project SET workflow_id = $2 WHERE id = $1`, projectID, workflowID); err != nil {
		t.Fatal(err)
	}
}

// The HTTP create resolves a step's handler before the service takes its
// locks. A workflow switch that commits in between must not leave the new issue
// with the former workflow's handler, nor without the handler the new workflow
// names, and the run that handler needs must exist.
func TestCreateIssueResolvesTheStepHandlerUnderTheProjectLocks(t *testing.T) {
	seedTestCatalog(t)
	former := seededReadyAgentID(t)
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, former).Scan(&runtimeID)
	latter := dbfx.Agent(t, "Create handoff latter "+workflowTestSuffix(), runtimeID)

	cases := []struct {
		name string
		// handler of in_progress in the workflow switched to; "" for none.
		handler string
	}{
		{"the new workflow names another handler", latter},
		{"the new workflow names no handler", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from := createTestWorkflow(t, "Create handler from "+workflowTestSuffix(), "in_progress")
			setWorkflowHandoff(t, from.ID, "in_progress", handToAgent(former, "former handler")).Want(http.StatusOK)
			to := createTestWorkflow(t, "Create handler to "+workflowTestSuffix(), "in_progress")
			if tc.handler != "" {
				setWorkflowHandoff(t, to.ID, "in_progress", handToAgent(tc.handler, "latter handler")).Want(http.StatusOK)
			}
			projectID := createWorkflowTestProject(t, "Create handler project")
			setProjectWorkflow(t, projectID, map[string]any{"workflow_id": from.ID}).Want(http.StatusOK)

			body := map[string]any{"title": "created mid switch " + workflowTestSuffix(), "status": "in_progress", "project_id": projectID}
			resp := raceAgainst(t, func(tx pgx.Tx) { switchProjectWorkflow(t, tx, projectID, to.ID) },
				func() *testutil.Response {
					return testutil.Call(t, testHandler.CreateIssue, newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, body))
				})
			resp.Want(http.StatusCreated)
			var created struct {
				ID string `json:"id"`
			}
			resp.JSON(&created)
			t.Cleanup(func() {
				testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, util.MustParseUUID(created.ID))
			})

			got := createdIssueAssignee(t, created.ID)
			if got == former {
				t.Fatalf("the issue kept the former workflow's handler %s", former)
			}
			if got != tc.handler {
				t.Fatalf("assignee = %q, want %q", got, tc.handler)
			}
			if tc.handler != "" {
				if steps := pendingStepRuns(t, created.ID, tc.handler); len(steps) != 1 {
					t.Fatalf("pending runs for the new handler = %v, want one", steps)
				}
			}
		})
	}
}

// A sub-issue created from a captured comment names no handler of its own. In a
// project whose step for the chosen status hands off, it gets that handler and
// its run, exactly as an ordinary create does.
func TestCommentSubIssueGetsTheStepHandlerAndRun(t *testing.T) {
	seedTestCatalog(t)
	syncIssueCounter(t)
	agentID := seededReadyAgentID(t)
	projectID := deliveryProject(t, "Comment sub-issue handoff project", agentID)
	sourceID := dbfx.Issue(t, "comment sub-issue source", testutil.Cols{"project_id": projectID, "status": "todo"})
	anchorID := dbfx.Comment(t, sourceID, "anchor comment")
	syncIssueCounter(t)

	build, err := service.BuildSourceContext(context.Background(), testHandler.Queries, util.MustParseUUID(testWorkspaceID), util.MustParseUUID(anchorID))
	if err != nil {
		t.Fatal(err)
	}
	req := withURLParam(newRequest("POST", "/api/comments/"+anchorID+"/sub-issues?workspace_id="+testWorkspaceID, map[string]any{
		"mode":          "manual",
		"capture_token": build.Token,
		"issue": map[string]any{
			"title":      "sub-issue from the anchor " + workflowTestSuffix(),
			"status":     "in_progress",
			"project_id": projectID,
		},
	}), "commentId", anchorID)
	resp := testutil.Call(t, testHandler.CreateCommentSubIssue, req)
	resp.Want(http.StatusCreated)
	var created struct {
		ID string `json:"id"`
	}
	resp.JSON(&created)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_source_context WHERE issue_id = $1`, util.MustParseUUID(created.ID))
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, util.MustParseUUID(created.ID))
	})

	if got := createdIssueAssignee(t, created.ID); got != agentID {
		t.Fatalf("assignee = %q, want the step's handler %s", got, agentID)
	}
	if steps := pendingStepRuns(t, created.ID, agentID); len(steps) != 1 {
		t.Fatalf("pending runs for the handler = %v, want one", steps)
	}
}

// The same create, with the project's workflow switched while it waits for the
// locks.
func TestCommentSubIssueResolvesTheStepHandlerUnderTheProjectLocks(t *testing.T) {
	seedTestCatalog(t)
	syncIssueCounter(t)
	former := seededReadyAgentID(t)
	projectID := deliveryProject(t, "Comment sub-issue switch project", former)
	to := createTestWorkflow(t, "Comment sub-issue to "+workflowTestSuffix(), "in_progress")
	sourceID := dbfx.Issue(t, "comment sub-issue switch source", testutil.Cols{"project_id": projectID, "status": "todo"})
	anchorID := dbfx.Comment(t, sourceID, "anchor comment")
	syncIssueCounter(t)
	build, err := service.BuildSourceContext(context.Background(), testHandler.Queries, util.MustParseUUID(testWorkspaceID), util.MustParseUUID(anchorID))
	if err != nil {
		t.Fatal(err)
	}

	req := withURLParam(newRequest("POST", "/api/comments/"+anchorID+"/sub-issues?workspace_id="+testWorkspaceID, map[string]any{
		"mode":          "manual",
		"capture_token": build.Token,
		"issue":         map[string]any{"title": "switched sub-issue " + workflowTestSuffix(), "status": "in_progress", "project_id": projectID},
	}), "commentId", anchorID)
	resp := raceAgainst(t, func(tx pgx.Tx) { switchProjectWorkflow(t, tx, projectID, to.ID) },
		func() *testutil.Response { return testutil.Call(t, testHandler.CreateCommentSubIssue, req) })
	resp.Want(http.StatusCreated)
	var created struct {
		ID string `json:"id"`
	}
	resp.JSON(&created)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_source_context WHERE issue_id = $1`, util.MustParseUUID(created.ID))
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, util.MustParseUUID(created.ID))
	})

	if got := createdIssueAssignee(t, created.ID); got != "" {
		t.Fatalf("assignee = %q, want none: the new workflow's step hands off to nobody", got)
	}
}
