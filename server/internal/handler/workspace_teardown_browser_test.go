package handler

import (
	"context"
	"testing"
)

// CHE-4873: browser_profile, browser_instance and browser_lease carry workspace_id but have no
// FK, so workspace deletion must remove them explicitly. The manifest classifies them
// workspaceDelete; this test proves the teardown does it.
func TestWorkspaceTeardownRemovesBrowserLeaseData(t *testing.T) {
	ctx := context.Background()
	user := dbfx.User(t, "Browser Teardown", "browser-teardown@multica.ai")
	doomed, neighbor := dbfx.Workspace(t, "Browser teardown", "browser-teardown"), dbfx.Workspace(t, "Browser neighbor", "browser-neighbor")
	for _, workspace := range []string{doomed, neighbor} {
		workspace := workspace
		dbfx.Exec(t, `INSERT INTO browser_profile (workspace_id, name, class) VALUES ($1, 'p', 'S0')`, workspace)
		dbfx.Exec(t, `INSERT INTO browser_instance (workspace_id, name) VALUES ($1, 'i')`, workspace)
		dbfx.Exec(t, `INSERT INTO browser_lease (workspace_id, instance_id, profile_id, agent_id, task_id, issue_id, user_id, class, state, expires_at)
			VALUES ($1, gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), $2, 'S0', 'queued', now() + interval '1 hour')`, workspace, user)
		t.Cleanup(func() {
			for _, table := range []string{"browser_lease", "browser_instance", "browser_profile"} {
				dbfx.Exec(t, `DELETE FROM `+table+` WHERE workspace_id = $1`, workspace)
			}
		})
	}
	if err := testHandler.Queries.DeleteWorkspaceAdministration(ctx, parseUUID(doomed)); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"browser_lease", "browser_instance", "browser_profile"} {
		if n := dbfx.Count(t, `SELECT count(*) FROM `+table+` WHERE workspace_id = $1`, doomed); n != 0 {
			t.Errorf("%s rows left after workspace teardown = %d, want 0", table, n)
		}
		if n := dbfx.Count(t, `SELECT count(*) FROM `+table+` WHERE workspace_id = $1`, neighbor); n != 1 {
			t.Errorf("neighbor %s rows = %d, want 1", table, n)
		}
	}
}
