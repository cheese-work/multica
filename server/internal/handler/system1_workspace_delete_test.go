package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// system1_observation has no foreign key, so workspace deletion must remove
// its rows itself (CHE-3628) and leave other workspaces' rows alone.
func TestDeleteWorkspace_RemovesSystem1Observations(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	doomed := dbfx.Insert(t, "workspace", testutil.Cols{"name": "System1 delete", "slug": "handler-delete-system1"})
	kept := dbfx.Insert(t, "workspace", testutil.Cols{"name": "System1 kept", "slug": "handler-delete-system1-kept"})
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM system1_observation WHERE workspace_id IN ($1, $2)`, doomed, kept)
		testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, kept)
	})
	dbfx.Exec(t, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`, doomed, testUserID)
	for _, ws := range []string{doomed, kept} {
		for _, kind := range []string{"decision", "routing"} {
			dbfx.Exec(t, `INSERT INTO system1_observation (workspace_id, kind, subject_id, class) VALUES ($1, $2, $3, 'reversible')`,
				ws, kind, uuid.NewString())
		}
	}

	req := withURLParam(newRequest(http.MethodDelete, "/api/workspaces/"+doomed, nil), "id", doomed)
	testutil.Call(t, testHandler.DeleteWorkspace, req).Want(http.StatusNoContent)

	count := func(ws string) (n int) {
		if err := testPool.QueryRow(ctx, `SELECT count(*) FROM system1_observation WHERE workspace_id = $1`, ws).Scan(&n); err != nil {
			t.Fatalf("count observations: %v", err)
		}
		return n
	}
	if n := count(doomed); n != 0 {
		t.Errorf("deleted workspace kept %d system1_observation rows", n)
	}
	if n := count(kept); n != 2 {
		t.Errorf("retained workspace has %d system1_observation rows, want 2", n)
	}
}

// A routing capture runs after the issue write, outside the deletion
// transaction. It must wait for a deleting workspace and then insert nothing,
// or its row outlives the workspace (no FK, CHE-3628).
func TestSystem1RoutingCaptureWaitsForWorkspaceDeletion(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	doomed := dbfx.Insert(t, "workspace", testutil.Cols{"name": "System1 race", "slug": "handler-system1-race"})
	kept := dbfx.Insert(t, "workspace", testutil.Cols{"name": "System1 race kept", "slug": "handler-system1-race-kept"})
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM system1_observation WHERE workspace_id IN ($1, $2)`, doomed, kept)
		testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id IN ($1, $2)`, doomed, kept)
	})
	capture := func(ws string) <-chan struct{} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			issue := db.Issue{ID: parseUUID(uuid.NewString()), WorkspaceID: parseUUID(ws), Status: "agent_review"}
			handoff := &workflowHandoff{def: issueworkflow.Definition{Name: "Default"}, step: issueworkflow.Step{StatusKey: "agent_review"}}
			testHandler.recordSystem1Routing(ctx, db.Issue{}, issue, handoff, "agent")
		}()
		return done
	}
	count := func(ws string) (n int) {
		if err := testPool.QueryRow(ctx, `SELECT count(*) FROM system1_observation WHERE workspace_id = $1`, ws).Scan(&n); err != nil {
			t.Fatalf("count observations: %v", err)
		}
		return n
	}

	// The deletion transaction: same lock DeleteWorkspace takes first.
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT id FROM workspace WHERE id = $1 FOR UPDATE`, doomed); err != nil {
		t.Fatal(err)
	}

	blocked := capture(doomed)
	free := capture(kept)
	select {
	case <-free:
	case <-time.After(5 * time.Second):
		t.Fatal("capture in an unrelated workspace was blocked")
	}
	select {
	case <-blocked:
		t.Fatal("capture finished while the workspace deletion held its lock")
	case <-time.After(500 * time.Millisecond):
	}

	if _, err := tx.Exec(ctx, `DELETE FROM system1_observation WHERE workspace_id = $1`, doomed); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM workspace WHERE id = $1`, doomed); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("capture never finished after the deletion committed")
	}
	if n := count(doomed); n != 0 {
		t.Errorf("capture left %d rows for the deleted workspace", n)
	}
	if n := count(kept); n != 1 {
		t.Errorf("retained workspace has %d rows, want 1", n)
	}
}
