package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
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
