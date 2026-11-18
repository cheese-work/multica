package migrations

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// CHE-6987 (plan CHE-6971, SA-1): the wakeup item kind on task_pr_event_delivery.
// Runs the real migration files against TEMP copies of the two pre-migration
// tables (a temp table shadows the real one in an unqualified statement), inside
// a transaction that is rolled back, so no shared table is locked or changed.
func TestTaskPREventWakeupItemMigration(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("integration test requires DATABASE_URL")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	// The shape migrations 769 and 771 left behind.
	if _, err := tx.Exec(ctx, `
		CREATE TEMP TABLE task_pr_event_capability (
			task_id UUID NOT NULL, workspace_id UUID NOT NULL, issue_id UUID NOT NULL,
			capability TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now());
		CREATE TEMP TABLE task_pr_event_delivery (
			task_id UUID NOT NULL, event_id UUID NOT NULL, workspace_id UUID NOT NULL, issue_id UUID NOT NULL,
			status TEXT NOT NULL CONSTRAINT task_pr_event_delivery_status_check CHECK (status IN ('pending', 'delivering', 'delivered', 'failed')),
			failure_reason TEXT, attempt_count INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(), delivered_at TIMESTAMPTZ);
		INSERT INTO task_pr_event_delivery (task_id, event_id, workspace_id, issue_id, status)
			VALUES (gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 'delivered');
		INSERT INTO task_pr_event_capability (task_id, workspace_id, issue_id, capability)
			VALUES (gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 'task-supplement-pr-event-v1');
	`); err != nil {
		t.Fatal(err)
	}

	applyMigrationFile(t, ctx, tx, "999l_task_pr_event_wakeup_item.up.sql")
	applyMigrationFile(t, ctx, tx, "999m_task_pr_event_wakeup_item_validate.up.sql")

	var kind string
	if err := tx.QueryRow(ctx, `SELECT item_kind FROM task_pr_event_delivery`).Scan(&kind); err != nil || kind != "pr_event" {
		t.Fatalf("existing item kind = %q, %v; want pr_event", kind, err)
	}

	const ids = `gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid()`
	const wakeupCols = `, wakeup_id, wakeup_revision, receipt_ids, note)`
	rejected := func(name, tail string) {
		t.Helper()
		if _, err := tx.Exec(ctx, "SAVEPOINT s"); err != nil {
			t.Fatal(err)
		}
		_, err := tx.Exec(ctx, `INSERT INTO task_pr_event_delivery (task_id, event_id, workspace_id, issue_id, status, item_kind`+tail)
		if !isCheckViolation(err) {
			t.Errorf("%s: error = %v, want a check violation", name, err)
		}
		if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT s"); err != nil {
			t.Fatal(err)
		}
	}
	accepted := func(name, tail string) {
		t.Helper()
		if _, err := tx.Exec(ctx, `INSERT INTO task_pr_event_delivery (task_id, event_id, workspace_id, issue_id, status, item_kind`+tail); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	// PR items: no fallback status, no wakeup columns.
	rejected("PR item with fallback",
		`) VALUES (`+ids+`, 'fallback', 'pr_event')`)
	rejected("PR item with wakeup_id",
		`, wakeup_id) VALUES (`+ids+`, 'pending', 'pr_event', gen_random_uuid())`)
	rejected("PR item with receipt_ids",
		`, receipt_ids) VALUES (`+ids+`, 'pending', 'pr_event', ARRAY[gen_random_uuid()])`)
	rejected("PR item with note",
		`, note) VALUES (`+ids+`, 'pending', 'pr_event', 'x')`)
	// Wakeup items need their wakeup columns and a non-empty receipt list.
	rejected("wakeup item without wakeup columns", `) VALUES (`+ids+`, 'pending', 'wakeup')`)
	rejected("wakeup item with empty receipt_ids", wakeupCols+` VALUES (`+ids+`, 'pending', 'wakeup', gen_random_uuid(), 1, ARRAY[]::uuid[], 'n')`)
	rejected("wakeup item without wakeup_revision", `, wakeup_id, receipt_ids, note) VALUES (`+ids+`, 'pending', 'wakeup', gen_random_uuid(), ARRAY[gen_random_uuid()], 'n')`)
	rejected("wakeup item without wakeup_id", `, wakeup_revision, receipt_ids, note) VALUES (`+ids+`, 'pending', 'wakeup', 1, ARRAY[gen_random_uuid()], 'n')`)
	// NULL passes a CHECK (UNKNOWN), so each of these needs its own IS NOT NULL.
	rejected("wakeup item without receipt_ids", `, wakeup_id, wakeup_revision, note) VALUES (`+ids+`, 'pending', 'wakeup', gen_random_uuid(), 1, 'n')`)
	rejected("wakeup item without note", `, wakeup_id, wakeup_revision, receipt_ids) VALUES (`+ids+`, 'pending', 'wakeup', gen_random_uuid(), 1, ARRAY[gen_random_uuid()])`)
	rejected("unknown item kind", `) VALUES (`+ids+`, 'pending', 'other')`)
	rejected("unknown status", wakeupCols+` VALUES (`+ids+`, 'bogus', 'wakeup', gen_random_uuid(), 1, ARRAY[gen_random_uuid()], 'n')`)
	// Accepted shapes.
	accepted("PR item", `) VALUES (`+ids+`, 'pending', 'pr_event')`)
	for _, status := range []string{"pending", "delivering", "delivered", "failed", "fallback"} {
		accepted("wakeup item "+status, wakeupCols+` VALUES (`+ids+`, '`+status+`', 'wakeup', gen_random_uuid(), 1, ARRAY[gen_random_uuid()], 'n')`)
	}

	// A v2 capability row is what the down migration must rewrite.
	if _, err := tx.Exec(ctx, `INSERT INTO task_pr_event_capability (task_id, workspace_id, issue_id, capability)
		VALUES (gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 'task-supplement-pr-event-v2')`); err != nil {
		t.Fatal(err)
	}

	applyMigrationFile(t, ctx, tx, "999m_task_pr_event_wakeup_item_validate.down.sql")
	applyMigrationFile(t, ctx, tx, "999l_task_pr_event_wakeup_item.down.sql")

	var rows, kept int
	if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE status IN ('pending', 'delivered'))
		FROM task_pr_event_delivery`).Scan(&rows, &kept); err != nil {
		t.Fatal(err)
	}
	if rows != 2 || kept != 2 {
		t.Fatalf("after down: %d items (%d pending or delivered), want the 2 PR items and no wakeup item", rows, kept)
	}
	var caps, v1 int
	if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE capability = 'task-supplement-pr-event-v1')
		FROM task_pr_event_capability`).Scan(&caps, &v1); err != nil {
		t.Fatal(err)
	}
	if caps != 2 || v1 != 2 {
		t.Fatalf("after down: %d capability rows, %d v1; want only v1 rows", caps, v1)
	}
	var columns int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = (SELECT nspname FROM pg_namespace WHERE oid = pg_my_temp_schema())
		  AND table_name = 'task_pr_event_delivery'
		  AND column_name IN ('item_kind', 'wakeup_id', 'wakeup_revision', 'receipt_ids', 'note')`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 0 {
		t.Fatalf("after down: %d new columns remain, want 0", columns)
	}
	// The original status CHECK is back: fallback is refused again.
	if _, err := tx.Exec(ctx, "SAVEPOINT s"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO task_pr_event_delivery (task_id, event_id, workspace_id, issue_id, status)
		VALUES (`+ids+`, 'fallback')`); !isCheckViolation(err) {
		t.Fatalf("fallback after down: error = %v, want a check violation", err)
	}
	if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT s"); err != nil {
		t.Fatal(err)
	}
	// And the up migration applies again on the rolled-back shape.
	applyMigrationFile(t, ctx, tx, "999l_task_pr_event_wakeup_item.up.sql")
	applyMigrationFile(t, ctx, tx, "999m_task_pr_event_wakeup_item_validate.up.sql")
}
