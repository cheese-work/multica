package handler

import (
	"context"
	"testing"
)

// CHE-4873, CHE-4737: browser_profile, browser_instance, browser_lease and the two permission tables carry workspace_id but have no
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
		dbfx.Exec(t, `INSERT INTO browser_profile_member (workspace_id, profile_id, user_id) VALUES ($1, gen_random_uuid(), $2)`, workspace, user)
		dbfx.Exec(t, `INSERT INTO browser_profile_agent (workspace_id, profile_id, agent_id) VALUES ($1, gen_random_uuid(), gen_random_uuid())`, workspace)
		t.Cleanup(func() {
			for _, table := range []string{"browser_lease", "browser_instance", "browser_profile", "browser_profile_member", "browser_profile_agent"} {
				dbfx.Exec(t, `DELETE FROM `+table+` WHERE workspace_id = $1`, workspace)
			}
		})
	}
	if err := testHandler.Queries.DeleteWorkspaceAdministration(ctx, parseUUID(doomed)); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"browser_lease", "browser_instance", "browser_profile", "browser_profile_member", "browser_profile_agent"} {
		if n := dbfx.Count(t, `SELECT count(*) FROM `+table+` WHERE workspace_id = $1`, doomed); n != 0 {
			t.Errorf("%s rows left after workspace teardown = %d, want 0", table, n)
		}
		if n := dbfx.Count(t, `SELECT count(*) FROM `+table+` WHERE workspace_id = $1`, neighbor); n != 1 {
			t.Errorf("neighbor %s rows = %d, want 1", table, n)
		}
	}
}
