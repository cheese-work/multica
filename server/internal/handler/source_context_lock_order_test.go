package handler

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// A source-context create locks the source issue, which is also the new
// issue's parent. A project workflow switch holds the project row and remaps
// the project's issues, the source issue among them. The create must reach the
// project lock before it holds the source issue, or each waits for the other
// and the database aborts one of them.
func TestSourceContextCreateTakesProjectLocksBeforeTheSourceIssue(t *testing.T) {
	seedTestCatalog(t)
	ctx := context.Background()
	projectID := createWorkflowTestProject(t, "Source context lock order")
	sourceID := dbfx.Issue(t, "source issue", testutil.Cols{"project_id": projectID, "status": "todo"})
	anchorID := dbfx.Comment(t, sourceID, "anchor comment")

	workspaceID := util.MustParseUUID(testWorkspaceID)
	build, err := service.BuildSourceContext(ctx, testHandler.Queries, workspaceID, util.MustParseUUID(anchorID))
	if err != nil {
		t.Fatal(err)
	}
	capture, err := service.PrepareSourceContextCapture(build, dbid.NewV7(), workspaceID, util.MustParseUUID(testUserID), time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}

	var createErr error
	var created string
	raceUntilBlocked(t, func(tx pgx.Tx) {
		// The switch's lock on the project.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM project WHERE id = $1 FOR UPDATE`, projectID); err != nil {
			t.Fatal(err)
		}
	}, func() {
		result, err := testHandler.IssueService.Create(ctx, service.IssueCreateParams{
			WorkspaceID: workspaceID, Title: "from the anchor comment " + workflowTestSuffix(), Status: "todo", Priority: "none",
			CreatorType: "member", CreatorID: util.MustParseUUID(testUserID),
			ParentIssueID: util.MustParseUUID(sourceID), SourceContext: &capture,
		}, service.IssueCreateOpts{})
		createErr = err
		created = util.UUIDToString(result.Issue.ID)
	}, func(tx pgx.Tx) {
		// The switch's remap of the project's issues, run while the create waits.
		// Under the wrong lock order the create already holds the source row and
		// this update waits for it: a deadlock.
		tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`)
		if _, err := tx.Exec(ctx, `UPDATE issue SET status = status WHERE id = $1`, util.MustParseUUID(sourceID)); err != nil {
			t.Errorf("the switch's remap of the source issue failed: %v", err)
		}
	})
	if createErr != nil {
		t.Fatalf("source-context create failed: %v", createErr)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_source_context WHERE issue_id = $1`, util.MustParseUUID(created))
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, util.MustParseUUID(created))
	})
}
