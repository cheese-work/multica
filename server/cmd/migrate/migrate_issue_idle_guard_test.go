package main

import (
	"context"
	"os"
	"testing"
	"time"
)

// The old guard wrote its receipt as free text into issue.metadata, so the
// migration must survive values that do not parse and metadata sitting at the
// 8192-byte column cap.
func TestIssueIdleGuardMigrationSurvivesLegacyMetadata(t *testing.T) {
	const valid = `{"state":"notified","kind":"idle","from":"in_progress","to":"blocked","attempts":2,"alert_id":"6f1c2c0e-1b7a-4c63-9d57-0a1b2c3d4e5f","moved_at":"2026-11-05T01:02:03Z"}`
	const unparseable = `{"state":"notified","kind":"idle","from":"in_progress","to":"blocked","attempts":"many","alert_id":"not-a-uuid","comment_id":"also-bad","idle_since":"not-a-time","moved_at":"2026-11-05T01:02:03Z","alert_attempts":"x"}`
	t.Run("unparseable_fields", func(t *testing.T) { runIdleGuardMigration(t, unparseable, false) })
	t.Run("metadata_at_the_size_cap", func(t *testing.T) { runIdleGuardMigration(t, valid, true) })
}

func runIdleGuardMigration(t *testing.T, receipt string, nearLimit bool) {
	t.Helper()
	admin := openTestPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	schema := createScratchSchema(t, ctx, admin, "idle_guard_migration_")
	pool := openTestPoolWithSearchPath(t, schema)
	for _, stmt := range []string{
		`CREATE TABLE workspace (id uuid PRIMARY KEY)`,
		`CREATE TABLE issue (
			id uuid PRIMARY KEY,
			workspace_id uuid NOT NULL,
			metadata jsonb NOT NULL DEFAULT '{}',
			CHECK (pg_column_size(metadata) <= 8192))`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("create fixture table: %v", err)
		}
	}
	const signal = `{"idle_guard_notified_to": "blocked"}`

	// With nearLimit, the pad is the smallest length where the metadata fits
	// with the receipt object but not with the object and the signal together.
	padLen := 0
	if nearLimit {
		if err := pool.QueryRow(ctx, `
			SELECT n FROM generate_series(7000, 8100) n
			WHERE pg_column_size(jsonb_build_object('pad', repeat('x', n)) || jsonb_build_object('idle_guard', $1::jsonb) || $2::jsonb) > 8192
			ORDER BY n LIMIT 1`, receipt, signal).Scan(&padLen); err != nil {
			t.Fatalf("find pad length: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `
		WITH w AS (INSERT INTO workspace VALUES (gen_random_uuid()) RETURNING id)
		INSERT INTO issue (id, workspace_id, metadata)
		SELECT gen_random_uuid(), w.id, jsonb_build_object('pad', repeat('x', $1::int)) || jsonb_build_object('idle_guard', $2::jsonb) FROM w`,
		padLen, receipt); err != nil {
		t.Fatalf("seed issue with a legacy receipt: %v", err)
	}

	sql, err := os.ReadFile(realMigrationFiles(t, []string{"961_issue_idle_guard"}, "up")[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("migration 961 up refused legacy metadata: %v", err)
	}

	var state, kind string
	var movedSet bool
	if err := pool.QueryRow(ctx, `SELECT state, kind, moved_at IS NOT NULL FROM issue_idle_guard`).Scan(&state, &kind, &movedSet); err != nil {
		t.Fatalf("read migrated receipt: %v", err)
	}
	if state != "notified" || kind != "idle" || !movedSet {
		t.Errorf("valid fields lost: state=%q kind=%q moved_at set=%t", state, kind, movedSet)
	}
	var badLeft bool
	if err := pool.QueryRow(ctx, `
		SELECT attempts <> 0 OR alert_attempts <> 0 OR comment_id IS NOT NULL OR idle_since IS NOT NULL
		FROM issue_idle_guard`).Scan(&badLeft); err != nil {
		t.Fatal(err)
	}
	if badLeft && !nearLimit {
		t.Errorf("unparseable fields must migrate as empty")
	}
	var hasObject bool
	var notifiedTo string
	if err := pool.QueryRow(ctx, `
		SELECT metadata ? 'idle_guard', COALESCE(metadata ->> 'idle_guard_notified_to', '') FROM issue`).Scan(&hasObject, &notifiedTo); err != nil {
		t.Fatalf("read migrated metadata: %v", err)
	}
	if hasObject || notifiedTo != "blocked" {
		t.Errorf("metadata object present=%t signal=%q, want object removed and signal blocked", hasObject, notifiedTo)
	}
}
