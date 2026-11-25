package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// CHE-9684 (A5e P2 2): CreateAutopilot checked the archive bit before its own
// transaction, so an archive that had begun but not committed was invisible to
// it. The create then inserted an active autopilot the archive's pause sweep
// could not see, and the project ended up archived with an active autopilot.
//
// The test holds the archive transaction open (archived, sweep done, not yet
// committed) and runs the create against it. A correct create waits for the
// archive, then refuses; it never leaves an active autopilot behind.
func TestCreateAutopilotSerializesWithProjectArchive(t *testing.T) {
	ctx := context.Background()
	project := createProjectPermissionTestProject(t, "autopilot archive race project")
	runtimeID := createCascadeFixtureRuntime(t, ctx, "Archive Race Runtime")
	agentID := createCascadeFixtureAgent(t, ctx, runtimeID, "Archive Race Agent")
	title := "archive race autopilot " + project.ID
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = testPool.Exec(bg, `DELETE FROM autopilot_run WHERE autopilot_id IN (SELECT id FROM autopilot WHERE title = $1)`, title)
		_, _ = testPool.Exec(bg, `DELETE FROM autopilot WHERE title = $1`, title)
	})

	archiveTx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer archiveTx.Rollback(ctx)
	if _, err := archiveTx.Exec(ctx,
		`UPDATE project SET archived_at = now() WHERE id = $1 AND workspace_id = $2`, project.ID, testWorkspaceID); err != nil {
		t.Fatal(err)
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		testHandler.CreateAutopilot(w, newRequest(http.MethodPost, "/api/autopilots?workspace_id="+testWorkspaceID, map[string]any{
			"title":                title,
			"assignee_id":          agentID,
			"execution_mode":       "create_issue",
			"issue_title_template": title,
			"project_id":           project.ID,
		}))
		done <- w
	}()

	// The create must be blocked behind the archive's row lock. If it answers
	// first, it decided from a pre-archive snapshot.
	select {
	case w := <-done:
		t.Fatalf("CreateAutopilot returned %d before the archive committed; it must wait for the project row: %s", w.Code, w.Body.String())
	case <-time.After(750 * time.Millisecond):
	}

	if err := archiveTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var w *httptest.ResponseRecorder
	select {
	case w = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("CreateAutopilot did not finish after the archive committed")
	}
	if w.Code != http.StatusConflict {
		t.Fatalf("CreateAutopilot after archive = %d, want 409: %s", w.Code, w.Body.String())
	}
	if code := decodeErrorCode(t, w); code != projectArchivedCode {
		t.Fatalf("error code = %q, want %q", code, projectArchivedCode)
	}
	var active int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM autopilot WHERE project_id = $1 AND status = 'active'`, project.ID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("%d active autopilot(s) left on an archived project", active)
	}
}
