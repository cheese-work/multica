package main

import (
	"context"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLaneSummaryMigrationLockTimeout(test *testing.T) {
	files := realMigrationFiles(test, []string{"596_chat_session_lane_summary"}, "up")
	sql, err := os.ReadFile(files[0])
	if err != nil {
		test.Fatal(err)
	}
	if !strings.HasPrefix(string(sql), "SET LOCAL lock_timeout = '2s';") {
		test.Fatal("lane-summary DDL must set a transaction-local 2-second lock timeout before altering chat_session")
	}
}

func TestLaneSummaryMigrationUpDownUp(test *testing.T) {
	base := openTestPool(test)
	ctx, cancel := context.WithTimeout(test.Context(), 30*time.Second)
	defer cancel()
	schema := createScratchSchema(test, ctx, base, "lane_summary_")
	pool := openTestPoolWithSearchPath(test, schema)
	if _, err := pool.Exec(ctx, `CREATE TABLE chat_session (id INTEGER PRIMARY KEY); INSERT INTO chat_session (id) VALUES (1)`); err != nil {
		test.Fatal(err)
	}
	for _, direction := range []string{"up", "down", "up"} {
		if err := runMigrations(ctx, pool, runOptions{
			Direction: direction, Files: realMigrationFiles(test, []string{"596_chat_session_lane_summary"}, direction),
			SchemaMigrationsTable: schema + ".schema_migrations", AdvisoryLockKey: int64(rand.Uint64()&0x7fffffffffffffff) | 1,
			Hooks: hooksForDirection(direction), Conditions: conditionsForDirection(direction),
		}); err != nil {
			test.Fatalf("migrate %s: %v", direction, err)
		}
		if direction == "down" {
			var count int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_session`).Scan(&count); err != nil || count != 1 {
				test.Fatalf("rollback lost legacy session: count=%d, err=%v", count, err)
			}
			continue
		}
		var summary string
		var updatedAt *time.Time
		if err := pool.QueryRow(ctx, `SELECT lane_summary, lane_summary_updated_at FROM chat_session WHERE id=1`).Scan(&summary, &updatedAt); err != nil || summary != "" || updatedAt != nil {
			test.Fatalf("legacy session defaults: %q, %v, %v", summary, updatedAt, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE chat_session SET lane_summary=$1 WHERE id=1`, strings.Repeat("🙂", 600)); err != nil {
			test.Fatalf("600 Unicode characters refused: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE chat_session SET lane_summary=$1 WHERE id=1`, strings.Repeat("🙂", 601)); err == nil {
			test.Fatal("601 Unicode characters accepted")
		}
	}
}
