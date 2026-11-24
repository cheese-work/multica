package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// CHE-9254 stage 1: issue_handoff_operation (CHE-5491) has no FK to workspace,
// so a workspace delete must remove its rows with an explicit statement.
func TestWorkspaceDeleteRemovesIssueHandoffOperations(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	f := newWorkspaceDeletePathFixture(t, "handoff-operation")
	ctx := t.Context()
	receipt := func(workspaceID string) {
		t.Helper()
		if _, err := testPool.Exec(ctx, `
INSERT INTO issue_handoff_operation (workspace_id, issue_id, operation_key, actor_type, request_hash)
VALUES ($1, gen_random_uuid(), gen_random_uuid(), 'agent', 'hash')
`, workspaceID); err != nil {
			t.Fatalf("insert issue_handoff_operation: %v", err)
		}
	}
	receipt(f.victimID)
	receipt(f.neighbourID)

	w := httptest.NewRecorder()
	testHandler.DeleteWorkspace(w, withURLParam(newRequest(http.MethodDelete, "/api/workspaces/"+f.victimID, nil), "id", f.victimID))
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete workspace = %d %s, want 204", w.Code, w.Body.String())
	}

	count := func(workspaceID string) (n int) {
		t.Helper()
		if err := testPool.QueryRow(ctx, `SELECT count(*) FROM issue_handoff_operation WHERE workspace_id = $1`, workspaceID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(f.victimID); n != 0 {
		t.Errorf("victim workspace left %d issue_handoff_operation rows", n)
	}
	if n := count(f.neighbourID); n != 1 {
		t.Errorf("neighbour workspace has %d issue_handoff_operation rows, want 1", n)
	}
}
