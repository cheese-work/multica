package migrations

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

const threadFocusUp = "999s_thread_focus_columns.up.sql"
const threadFocusDown = "999s_thread_focus_columns.down.sql"

// CHE-6426 (S1 of CHE-6385): runs the real up and down files against TEMP copies
// of the three tables (a temp table shadows the real one), inside a transaction
// that is rolled back, so no shared table is locked or changed.
func TestThreadFocusColumnsMigration(t *testing.T) {
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

	// The shape before the migration.
	if _, err := tx.Exec(ctx, `
		CREATE TEMP TABLE comment (id UUID PRIMARY KEY);
		CREATE TEMP TABLE decision_request (id UUID PRIMARY KEY);
		CREATE TEMP TABLE ask_guard_event (
			id UUID PRIMARY KEY,
			mode TEXT NOT NULL CONSTRAINT ask_guard_event_mode_check CHECK (mode IN ('warn', 'block')));
		INSERT INTO comment (id) VALUES (gen_random_uuid());
		INSERT INTO decision_request (id) VALUES (gen_random_uuid());
		INSERT INTO ask_guard_event (id, mode) VALUES (gen_random_uuid(), 'block');
	`); err != nil {
		t.Fatal(err)
	}

	columns := func() map[string]bool {
		rows, err := tx.Query(ctx, `
			SELECT table_name || '.' || column_name FROM information_schema.columns
			WHERE table_schema LIKE 'pg_temp%' AND (
				(table_name = 'comment' AND column_name IN ('notify_only', 'ask_source_root_id')) OR
				(table_name = 'decision_request' AND column_name = 'reask_of_request_id'))`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		found := map[string]bool{}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			found[name] = true
		}
		return found
	}
	insertMode := func(mode string) error {
		if _, err := tx.Exec(ctx, "SAVEPOINT s"); err != nil {
			t.Fatal(err)
		}
		_, err := tx.Exec(ctx, `INSERT INTO ask_guard_event (id, mode) VALUES (gen_random_uuid(), $1)`, mode)
		if err != nil {
			if _, rbErr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT s"); rbErr != nil {
				t.Fatal(rbErr)
			}
		}
		return err
	}
	checkState := func() (validated bool) {
		if err := tx.QueryRow(ctx, `SELECT convalidated FROM pg_constraint WHERE conrelid = 'ask_guard_event'::regclass AND conname = 'ask_guard_event_mode_check'`).Scan(&validated); err != nil {
			t.Fatalf("ask_guard_event_mode_check missing: %v", err)
		}
		return validated
	}

	if err := insertMode("move"); !isCheckViolation(err) {
		t.Fatalf("before up: move insert error = %v, want a check violation", err)
	}

	applyMigrationFile(t, ctx, tx, threadFocusUp)

	if got := columns(); len(got) != 3 {
		t.Fatalf("columns after up = %v, want the three new ones", got)
	}
	var nulls int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM comment WHERE notify_only IS NULL AND ask_source_root_id IS NULL`).Scan(&nulls); err != nil || nulls != 1 {
		t.Fatalf("existing comment rows with NULL columns = %d, %v; want 1", nulls, err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM decision_request WHERE reask_of_request_id IS NULL`).Scan(&nulls); err != nil || nulls != 1 {
		t.Fatalf("existing request rows with NULL reask_of_request_id = %d, %v; want 1", nulls, err)
	}
	for _, mode := range []string{"warn", "block", "move", "refused"} {
		if err := insertMode(mode); err != nil {
			t.Errorf("after up: mode %q rejected: %v", mode, err)
		}
	}
	if err := insertMode("bogus"); !isCheckViolation(err) {
		t.Errorf("after up: bogus mode error = %v, want a check violation", err)
	}
	if checkState() {
		t.Error("widened CHECK is validated; want NOT VALID")
	}

	applyMigrationFile(t, ctx, tx, threadFocusDown)

	if got := columns(); len(got) != 0 {
		t.Fatalf("columns after down = %v, want none", got)
	}
	if err := insertMode("move"); !isCheckViolation(err) {
		t.Errorf("after down: move insert error = %v, want a check violation", err)
	}
	if err := insertMode("block"); err != nil {
		t.Errorf("after down: block rejected: %v", err)
	}
	if checkState() {
		t.Error("restored CHECK is validated; want NOT VALID")
	}
}

// The real table, as it stands after every migration ran.
func TestAskGuardEventAcceptsMoveAndRefused(t *testing.T) {
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

	for _, mode := range []string{"warn", "block", "move", "refused"} {
		if _, err := tx.Exec(ctx, "SAVEPOINT s"); err != nil {
			t.Fatal(err)
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO ask_guard_event (id, workspace_id, issue_id, task_id, operation_kind, payload_sha256, mode, root_id)
			VALUES (gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 'thread_focus_test', 'x', $1, gen_random_uuid())`, mode)
		if err != nil {
			t.Errorf("mode %q rejected on the real table: %v", mode, err)
			if _, rbErr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT s"); rbErr != nil {
				t.Fatal(rbErr)
			}
		}
	}
}
