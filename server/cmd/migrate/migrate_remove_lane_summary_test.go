package main

import (
	"context"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRemoveLaneSummaryMigrationLockTimeout(test *testing.T) {
	for _, direction := range []string{"up", "down"} {
		files := realMigrationFiles(test, []string{"619_remove_chat_session_lane_summary"}, direction)
		sql, err := os.ReadFile(files[0])
		if err != nil {
			test.Fatal(err)
		}
		if !strings.HasPrefix(string(sql), "SET LOCAL lock_timeout = '2s';") {
			test.Fatalf("%s DDL must bound chat_session lock acquisition", direction)
		}
	}
}

func TestRemoveLaneSummaryMigrationUpDownUp(test *testing.T) {
	base := openTestPool(test)
	ctx, cancel := context.WithTimeout(test.Context(), 30*time.Second)
	defer cancel()
	schema := createScratchSchema(test, ctx, base, "remove_lane_summary_")
	pool := openTestPoolWithSearchPath(test, schema)
	if _, err := pool.Exec(ctx, `CREATE TABLE chat_session (id INTEGER PRIMARY KEY); INSERT INTO chat_session (id) VALUES (1)`); err != nil {
		test.Fatal(err)
	}
	apply := func(name, direction string) {
		test.Helper()
		if err := runMigrations(ctx, pool, runOptions{
			Direction: direction, Files: realMigrationFiles(test, []string{name}, direction),
			SchemaMigrationsTable: schema + ".schema_migrations", AdvisoryLockKey: int64(rand.Uint64()&0x7fffffffffffffff) | 1,
			Hooks: hooksForDirection(direction), Conditions: conditionsForDirection(direction),
		}); err != nil {
			test.Fatalf("migrate %s %s: %v", name, direction, err)
		}
	}
	apply("596_chat_session_lane_summary", "up")
	if _, err := pool.Exec(ctx, `UPDATE chat_session SET lane_summary = 'Existing summary', lane_summary_updated_at = now() WHERE id = 1`); err != nil {
		test.Fatal(err)
	}
	for _, direction := range []string{"up", "down", "up"} {
		apply("619_remove_chat_session_lane_summary", direction)
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_session WHERE id = 1`).Scan(&count); err != nil || count != 1 {
			test.Fatalf("%s lost the session: count=%d, err=%v", direction, count, err)
		}
		if direction == "up" {
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema = $1 AND table_name = 'chat_session' AND column_name IN ('lane_summary', 'lane_summary_updated_at')`, schema).Scan(&count); err != nil || count != 0 {
				test.Fatalf("removed columns remain: count=%d, err=%v", count, err)
			}
			continue
		}
		var summary string
		var updatedAt *time.Time
		if err := pool.QueryRow(ctx, `SELECT lane_summary, lane_summary_updated_at FROM chat_session WHERE id = 1`).Scan(&summary, &updatedAt); err != nil || summary != "" || updatedAt != nil {
			test.Fatalf("rollback must re-add empty columns: %q, %v, %v", summary, updatedAt, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE chat_session SET lane_summary = $1 WHERE id = 1`, strings.Repeat("🙂", 601)); err == nil {
			test.Fatal("rollback did not restore the summary length constraint")
		}
	}
}
