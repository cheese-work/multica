package handler

import (
	"context"
	"testing"
)

// ops_snapshot, ops_snapshot_5m and agent_task_event carry no FK to workspace
// (migration 859), so DeleteWorkspace must sweep them itself.
func TestOps_WorkspaceDeleteRemovesRows(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ws := dbfx.Workspace(t, "Ops cleanup", "ops-cleanup")
	other := dbfx.Workspace(t, "Ops cleanup keep", "ops-cleanup-keep")
	for _, id := range []string{ws, other} {
		dbfx.Exec(t, `INSERT INTO ops_snapshot (workspace_id, daemon_id, bucket_ts) VALUES ($1, 'm1', now())`, id)
		dbfx.Exec(t, `INSERT INTO ops_snapshot_5m (workspace_id, daemon_id, bucket_ts, sample_count) VALUES ($1, 'm1', now(), 1)`, id)
		dbfx.Exec(t, `INSERT INTO agent_task_event (workspace_id, task_id, kind) VALUES ($1, gen_random_uuid(), 'stale_reclaim')`, id)
	}
	t.Cleanup(func() {
		for _, table := range []string{"ops_snapshot", "ops_snapshot_5m", "agent_task_event"} {
			_, _ = testPool.Exec(context.Background(), `DELETE FROM `+table+` WHERE workspace_id = $1`, other)
		}
	})

	if err := testHandler.Queries.DeleteWorkspace(context.Background(), parseUUID(ws)); err != nil {
		t.Fatalf("delete workspace: %v", err)
	}
	for _, table := range []string{"ops_snapshot", "ops_snapshot_5m", "agent_task_event"} {
		var gone, kept int
		dbfx.QueryRow(t, `SELECT count(*) FROM `+table+` WHERE workspace_id = $1`, ws).Scan(&gone)
		dbfx.QueryRow(t, `SELECT count(*) FROM `+table+` WHERE workspace_id = $1`, other).Scan(&kept)
		if gone != 0 {
			t.Errorf("%s rows after workspace delete = %d, want 0", table, gone)
		}
		if kept != 1 {
			t.Errorf("%s rows of the other workspace = %d, want 1", table, kept)
		}
	}
}
