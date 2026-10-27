package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// moveIssueToProject is a competing writer that moves the issue into a
// project and leaves its transaction open.
func moveIssueToProject(t *testing.T, tx pgx.Tx, issueID, projectID string) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), `UPDATE issue SET project_id = $2 WHERE id = $1`,
		util.MustParseUUID(issueID), util.MustParseUUID(projectID)); err != nil {
		t.Fatal(err)
	}
}

func issueProject(t *testing.T, issueID string) string {
	t.Helper()
	var projectID *string
	dbfx.QueryRow(t, `SELECT project_id::text FROM issue WHERE id = $1`, issueID).Scan(&projectID)
	if projectID == nil {
		return ""
	}
	return *projectID
}

// An issue write decides its workflow from the project it read, then locks and
// re-checks. A member who moves the issue to another project in between must
// win: the write may not validate the status under the project it read and
// persist it under the project the issue now sits in, nor move the issue back.
func TestIssueWriteFollowsTheProjectTheIssueMovedTo(t *testing.T) {
	seedTestCatalog(t)
	for _, writer := range issueWriters {
		for _, withDescription := range []bool{false, true} {
			name := writer.name
			if withDescription {
				name += " with description"
			}
			body := func() map[string]any {
				b := map[string]any{"status": "in_progress"}
				if withDescription {
					b["description"] = "edited with the status"
				}
				return b
			}

			t.Run("moved to a project whose workflow omits the status/"+name, func(t *testing.T) {
				listsIt := createTestWorkflow(t, "Lock move from "+workflowTestSuffix(), "in_progress")
				without := createTestWorkflow(t, "Lock move to "+workflowTestSuffix())
				from := createWorkflowTestProject(t, "Lock move source")
				to := createWorkflowTestProject(t, "Lock move target")
				setProjectWorkflow(t, from, map[string]any{"workflow_id": listsIt.ID}).Want(http.StatusOK)
				setProjectWorkflow(t, to, map[string]any{"workflow_id": without.ID}).Want(http.StatusOK)
				issueID := dbfx.Issue(t, "lock move", testutil.Cols{"project_id": from, "status": "todo"})

				resp := raceAgainst(t, func(tx pgx.Tx) { moveIssueToProject(t, tx, issueID, to) },
					func() *testutil.Response { return writer.write(t, issueID, body(), nil) })

				if got := issueProject(t, issueID); got != to {
					t.Fatalf("project = %s, want %s: the write undid the move", got, to)
				}
				wantIssueStatus(t, issueID, "todo")
				if writer.name == "single" {
					resp.Want(http.StatusConflict)
				}
			})

			t.Run("moved into a project from none/"+name, func(t *testing.T) {
				without := createTestWorkflow(t, "Lock none to "+workflowTestSuffix())
				to := createWorkflowTestProject(t, "Lock none target")
				setProjectWorkflow(t, to, map[string]any{"workflow_id": without.ID}).Want(http.StatusOK)
				issueID := dbfx.Issue(t, "lock none", testutil.Cols{"status": "todo"})

				resp := raceAgainst(t, func(tx pgx.Tx) { moveIssueToProject(t, tx, issueID, to) },
					func() *testutil.Response { return writer.write(t, issueID, body(), nil) })

				if got := issueProject(t, issueID); got != to {
					t.Fatalf("project = %q, want %s: the write undid the move", got, to)
				}
				wantIssueStatus(t, issueID, "todo")
				if writer.name == "single" {
					resp.Want(http.StatusConflict)
				}
			})
		}
	}
}

// Creating an issue resolves the project's workflow, then inserts. A workflow
// switch that commits in between must not leave the new issue on a status the
// new workflow omits, for built-in statuses as much as custom ones, and for a
// sub-issue that inherits its parent's project as much as for an explicit one.
func TestCreateIssueRechecksWorkflowSwitchedMidFlight(t *testing.T) {
	seedTestCatalog(t)
	for _, inherited := range []bool{false, true} {
		name := "explicit project"
		if inherited {
			name = "project inherited from the parent"
		}
		t.Run(name, func(t *testing.T) {
			listsIt := createTestWorkflow(t, "Create switch from "+workflowTestSuffix(), "in_progress")
			without := createTestWorkflow(t, "Create switch to "+workflowTestSuffix())
			projectID := createWorkflowTestProject(t, "Create race project")
			setProjectWorkflow(t, projectID, map[string]any{"workflow_id": listsIt.ID}).Want(http.StatusOK)

			body := map[string]any{"title": "created mid switch " + workflowTestSuffix(), "status": "in_progress"}
			if inherited {
				body["parent_issue_id"] = dbfx.Issue(t, "create race parent", testutil.Cols{"project_id": projectID, "status": "todo"})
			} else {
				body["project_id"] = projectID
			}

			resp := raceAgainst(t, func(tx pgx.Tx) {
				if _, err := tx.Exec(context.Background(), `SELECT 1 FROM project WHERE id = $1 FOR UPDATE`, projectID); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(context.Background(), `UPDATE project SET workflow_id = $2 WHERE id = $1`, projectID, without.ID); err != nil {
					t.Fatal(err)
				}
			}, func() *testutil.Response {
				return testutil.Call(t, testHandler.CreateIssue, newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, body))
			})

			if resp.Code != http.StatusCreated {
				return // a refusal leaves no issue outside the workflow either
			}
			var created struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			}
			resp.JSON(&created)
			t.Cleanup(func() {
				testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, util.MustParseUUID(created.ID))
			})
			if created.Status == "in_progress" {
				t.Fatalf("created on in_progress, a status the project's workflow no longer lists")
			}
			wantIssueStatus(t, created.ID, created.Status)
		})
	}
}
