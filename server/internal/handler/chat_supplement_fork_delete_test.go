package handler

import (
	"context"
	"testing"
)

// CHE-7535 / CHE-7278 S1: migration 999q00 keeps the fork's chat_task_supplement
// as chat_task_supplement_fork_875 for restore. The table has no FK, so only an
// explicit workspace-scoped delete removes its rows when a workspace is torn
// down. The manifest classifies it workspaceDelete; this proves the delete is
// real and stays inside the deleted workspace.
func TestWorkspaceTeardownRemovesRetainedForkChatTaskSupplements(t *testing.T) {
	ctx := context.Background()
	doomed := dbfx.Workspace(t, "Fork supplement teardown", "fork-supplement-teardown")
	neighbor := dbfx.Workspace(t, "Fork supplement neighbor", "fork-supplement-neighbor")
	for _, workspace := range []string{doomed, neighbor} {
		workspace := workspace
		dbfx.Exec(t, `INSERT INTO chat_task_supplement_fork_875 (message_id, task_id, workspace_id, chat_session_id, author_id, status)
			VALUES (gen_random_uuid(), gen_random_uuid(), $1, gen_random_uuid(), $2, 'delivered')`, workspace, testUserID)
		t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM chat_task_supplement_fork_875 WHERE workspace_id = $1`, workspace) })
	}
	if err := testHandler.Queries.DeleteWorkspaceLeafData(ctx, parseUUID(doomed)); err != nil {
		t.Fatal(err)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM chat_task_supplement_fork_875 WHERE workspace_id = $1`, doomed); n != 0 {
		t.Fatalf("retained fork rows left after workspace teardown = %d, want 0", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM chat_task_supplement_fork_875 WHERE workspace_id = $1`, neighbor); n != 1 {
		t.Fatalf("neighbor workspace retained fork rows = %d, want 1", n)
	}
}
