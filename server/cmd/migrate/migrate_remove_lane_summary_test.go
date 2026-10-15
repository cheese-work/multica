package main

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/multica-ai/multica/server/internal/migrations"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestAllMigrationsRemoveLaneSummaryAndRestoreEmptyColumns(test *testing.T) {
	base := openTestPool(test)
	ctx, cancel := context.WithTimeout(test.Context(), 2*time.Minute)
	defer cancel()
	files, err := migrations.Files("up")
	if err != nil {
		test.Fatal(err)
	}
	if err := runMigrations(ctx, base, runOptions{
		Direction: "up", Files: files,
		Hooks: hooksForDirection("up"), Conditions: conditionsForDirection("up"),
	}); err != nil {
		test.Fatalf("apply all migrations: %v", err)
	}
	var columnCount int
	if err := base.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'chat_session' AND column_name IN ('lane_summary', 'lane_summary_updated_at')`).Scan(&columnCount); err != nil {
		test.Fatal(err)
	}
	if columnCount != 0 {
		test.Fatalf("all migrations must remove retired lane-summary columns: found %d, want 0", columnCount)
	}

	var removalVersion string
	for _, filename := range files {
		if strings.HasSuffix(filename, "_remove_chat_session_lane_summary.up.sql") {
			removalVersion = migrations.ExtractVersion(filename)
		}
	}
	if removalVersion == "" {
		test.Fatal("all migrations must include a reversible lane-summary removal")
	}
	schema := createScratchSchema(test, ctx, base, "remove_lane_summary_")
	pool := openTestPoolWithSearchPath(test, schema)
	if _, err := pool.Exec(ctx, `CREATE TABLE chat_session (LIKE public.chat_session INCLUDING ALL)`); err != nil {
		test.Fatal(err)
	}
	fixture := testutil.New(pool, uuid.NewString(), uuid.NewString())
	sessionID := fixture.ChatSession(test, uuid.NewString())
	lockKey := int64(rand.Uint64()&0x7fffffffffffffff) | 1
	apply := func(direction string) {
		test.Helper()
		migrationFiles := realMigrationFiles(test, []string{removalVersion}, direction)
		sql, err := os.ReadFile(migrationFiles[0])
		if err != nil {
			test.Fatal(err)
		}
		if !strings.HasPrefix(string(sql), "SET LOCAL lock_timeout = '2s';") {
			test.Fatalf("%s DDL must bound chat_session lock acquisition", direction)
		}
		if err := runMigrations(ctx, pool, runOptions{
			Direction: direction, Files: migrationFiles,
			SchemaMigrationsTable: schema + ".schema_migrations", AdvisoryLockKey: lockKey,
		}); err != nil {
			test.Fatalf("migrate %s %s: %v", removalVersion, direction, err)
		}
	}
	for _, direction := range []string{"up", "down", "up"} {
		apply(direction)
		var rowCount int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_session WHERE id = $1`, sessionID).Scan(&rowCount); err != nil || rowCount != 1 {
			test.Fatalf("%s lost the session: count=%d, err=%v", direction, rowCount, err)
		}
		if direction == "up" {
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema = $1 AND table_name = 'chat_session' AND column_name IN ('lane_summary', 'lane_summary_updated_at')`, schema).Scan(&columnCount); err != nil || columnCount != 0 {
				test.Fatalf("removed columns remain: count=%d, err=%v", columnCount, err)
			}
			continue
		}
		var summary string
		var updatedAt *time.Time
		if err := pool.QueryRow(ctx, `SELECT lane_summary, lane_summary_updated_at FROM chat_session WHERE id = $1`, sessionID).Scan(&summary, &updatedAt); err != nil || summary != "" || updatedAt != nil {
			test.Fatalf("rollback must re-add empty columns: %q, %v, %v", summary, updatedAt, err)
		}
		fixture.Exec(test, `UPDATE chat_session SET lane_summary = $1, lane_summary_updated_at = now() WHERE id = $2`, strings.Repeat("🙂", 600), sessionID)
		_, err := pool.Exec(ctx, `UPDATE chat_session SET lane_summary = $1 WHERE id = $2`, strings.Repeat("🙂", 601), sessionID)
		var postgresError *pgconn.PgError
		if !errors.As(err, &postgresError) || postgresError.Code != "23514" || postgresError.ConstraintName != "chat_session_lane_summary_length" {
			test.Fatalf("rollback must restore the 600-character constraint: %v", err)
		}
	}
}
