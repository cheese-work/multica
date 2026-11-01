package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// CHE-1126 Layer 2: a workspace delete removes the workspace's recovery claims.
func TestWorkspaceDeleteRemovesRecoveryClaims(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	f := newWorkspaceDeletePathFixture(t, "recovery-claim")
	ctx := t.Context()
	claim := func(workspaceID string) {
		t.Helper()
		if _, err := testPool.Exec(ctx, `
INSERT INTO recovery_claim (workspace_id, issue_id, failed_task_id, lineage_root_task_id, mode, claimed_at)
VALUES ($1, gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 'fresh_replay', now())
`, workspaceID); err != nil {
			t.Fatalf("insert recovery_claim: %v", err)
		}
	}
	claim(f.victimID)
	claim(f.neighbourID)

	w := httptest.NewRecorder()
	testHandler.DeleteWorkspace(w, withURLParam(newRequest(http.MethodDelete, "/api/workspaces/"+f.victimID, nil), "id", f.victimID))
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete workspace = %d %s, want 204", w.Code, w.Body.String())
	}

	count := func(workspaceID string) (n int) {
		t.Helper()
		if err := testPool.QueryRow(ctx, `SELECT count(*) FROM recovery_claim WHERE workspace_id = $1`, workspaceID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(f.victimID); n != 0 {
		t.Errorf("victim workspace left %d recovery_claim rows", n)
	}
	if n := count(f.neighbourID); n != 1 {
		t.Errorf("neighbour workspace has %d recovery_claim rows, want 1", n)
	}
}
