package main

import (
	"context"
	"math/rand/v2"
	"testing"
	"time"
)

// TestTaskUsageHourlySquadDownKeepsPendingDirtyKeys proves 742 down does not
// lose pending recompute work for squad-attributed dirty keys (CHE-9438 H').
//
// A task delete queues only its old bucket (DELETE branch of
// enqueue_task_usage_hourly_dirty_for_atq). If the rollback runs before the
// worker handles that key, discarding the squad_id row would keep the stale
// hourly total with no later touch to correct it. 742 down must instead fold
// each pending squad key into its legacy key (squad_id = NULL), keep the latest
// enqueued_at, merge with an already-pending NULL-squad row, and still let 741
// down rebuild uq_task_usage_hourly_dirty_key without a uniqueness error.
func TestTaskUsageHourlySquadDownKeepsPendingDirtyKeys(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool, schema := newPre735TaskUsageSchema(ctx, t)

	versions := []string{
		"99zzz75_task_usage_hourly_squad_col",
		"99zzz76_task_usage_hourly_squad_uq_idx",
		"99zzz77_task_usage_hourly_dirty_squad_uq_idx",
		"99zzz78_task_usage_hourly_workspace_squad_idx",
		"99zzz79_task_usage_hourly_squad_pipeline",
		"99zzz80_task_usage_hourly_drop_legacy_key",
		"99zzz81_task_usage_hourly_dirty_drop_legacy_key",
		"99zzz82_task_usage_hourly_squad_rollback_merge",
	}
	reversed := make([]string, len(versions))
	for i := range versions {
		reversed[i] = versions[len(versions)-1-i]
	}
	runDir := func(dir string, list []string) {
		t.Helper()
		if err := runMigrations(ctx, pool, runOptions{
			Direction:             dir,
			Files:                 realMigrationFiles(t, list, dir),
			SchemaMigrationsTable: schema + ".schema_migrations",
			AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
			Hooks:                 hooksForDirection(dir),
		}); err != nil {
			t.Fatalf("run %s migrations: %v", dir, err)
		}
	}

	// Migrate up past 742.
	runDir("up", versions)

	const (
		wsID   = "00000000-0000-0000-0000-000000000001"
		rtID   = "00000000-0000-0000-0000-000000000002"
		agID   = "00000000-0000-0000-0000-000000000003"
		sq1ID  = "00000000-0000-0000-0000-000000000011"
		sq2ID  = "00000000-0000-0000-0000-000000000012"
		taskID = "00000000-0000-0000-0000-0000000000a1"
	)

	// Key A: a task delete queues its squad bucket; nothing touches it later.
	if _, err := pool.Exec(ctx, `INSERT INTO agent (id, workspace_id) VALUES ($1, $2)`, agID, wsID); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agent_task_queue (id, agent_id, runtime_id, squad_id) VALUES ($1, $2, $3, $4)`,
		taskID, agID, rtID, sq1ID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO task_usage (id, task_id, provider, model, input_tokens, created_at)
		VALUES ('00000000-0000-0000-0000-0000000000b1', $1, 'claude', 'model-a', 100, '2026-11-23 10:30:00+00')
	`, taskID); err != nil {
		t.Fatalf("seed task usage: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM agent_task_queue WHERE id = $1`, taskID); err != nil {
		t.Fatalf("delete task: %v", err)
	}
	var queued int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM task_usage_hourly_dirty
		 WHERE squad_id = $1 AND model = 'model-a' AND bucket_hour = '2026-11-23 10:00:00+00'
	`, sq1ID).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("task delete should queue one squad dirty key, got %d (err=%v)", queued, err)
	}
	// Pin enqueued_at so the assertion does not depend on now().
	if _, err := pool.Exec(ctx, `UPDATE task_usage_hourly_dirty SET enqueued_at = '2026-11-23 11:00:00+00' WHERE model = 'model-a'`); err != nil {
		t.Fatalf("pin key A enqueued_at: %v", err)
	}

	// Key B: two squad rows plus an already-pending NULL-squad row share one
	// legacy 7-column key; the squad rows hold the latest enqueued_at.
	// Key C: same shape, but the NULL-squad row is the latest.
	if _, err := pool.Exec(ctx, `
		INSERT INTO task_usage_hourly_dirty (
			bucket_hour, workspace_id, runtime_id, agent_id, project_id, squad_id, provider, model, enqueued_at
		) VALUES
			('2026-11-23 12:00:00+00', $1, $2, $3, NULL, $4,   'claude', 'model-b', '2026-11-23 12:10:00+00'),
			('2026-11-23 12:00:00+00', $1, $2, $3, NULL, $5,   'claude', 'model-b', '2026-11-23 12:30:00+00'),
			('2026-11-23 12:00:00+00', $1, $2, $3, NULL, NULL, 'claude', 'model-b', '2026-11-23 12:05:00+00'),
			('2026-11-23 13:00:00+00', $1, $2, $3, NULL, $4,   'claude', 'model-c', '2026-11-23 13:10:00+00'),
			('2026-11-23 13:00:00+00', $1, $2, $3, NULL, NULL, 'claude', 'model-c', '2026-11-23 13:50:00+00')
	`, wsID, rtID, agID, sq1ID, sq2ID); err != nil {
		t.Fatalf("seed dirty rows: %v", err)
	}

	// Roll back 742 and 741 (and the rest) with no worker tick in between.
	runDir("down", reversed)

	rows, err := pool.Query(ctx, `
		SELECT model, enqueued_at
		  FROM task_usage_hourly_dirty
		 ORDER BY model
	`)
	if err != nil {
		t.Fatalf("query dirty rows: %v", err)
	}
	defer rows.Close()
	got := map[string]time.Time{}
	for rows.Next() {
		var model string
		var at time.Time
		if err := rows.Scan(&model, &at); err != nil {
			t.Fatalf("scan dirty row: %v", err)
		}
		got[model] = at.UTC()
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate dirty rows: %v", err)
	}
	want := map[string]time.Time{
		"model-a": time.Date(2026, 11, 23, 11, 0, 0, 0, time.UTC),
		"model-b": time.Date(2026, 11, 23, 12, 30, 0, 0, time.UTC),
		"model-c": time.Date(2026, 11, 23, 13, 50, 0, 0, time.UTC),
	}
	if len(got) != len(want) {
		t.Fatalf("legacy dirty keys after down = %v, want exactly %v", got, want)
	}
	for model, wantAt := range want {
		gotAt, ok := got[model]
		if !ok {
			t.Fatalf("pending dirty key %s was lost by 742 down; got %v", model, got)
		}
		if !gotAt.Equal(wantAt) {
			t.Fatalf("dirty key %s enqueued_at = %s, want %s", model, gotAt, wantAt)
		}
	}

	// 741 down must have rebuilt the legacy unique index on the merged table.
	assertIndexValidity(t, pool, schema, "uq_task_usage_hourly_dirty_key", true)
}
