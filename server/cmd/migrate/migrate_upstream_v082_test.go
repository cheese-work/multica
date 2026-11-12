package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Upstream v0.8.2 shipped migrations 633-636; the fork renumbers them to
// 99936-99939 because 633-636 are fork-owned prefixes (CHE-5322).
func TestUpstreamV082Migrations(t *testing.T) {
	admin := openTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	schema := fmt.Sprintf("migrate_v082_%d_%d", time.Now().UnixNano(), rand.Uint32())
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Errorf("cleanup schema: %v", err)
		}
	})
	pool := openTestPoolWithSearchPath(t, schema)
	versions := []string{
		"99936_issue_environment",
		"99937_issue_environment_task_key_idx",
		"99938_issue_environment_issue_idx",
		"99939_site_counter",
	}
	indexes := map[string]string{
		versions[1]: "uq_issue_environment_task_key",
		versions[2]: "idx_issue_environment_issue",
	}
	for version, index := range indexes {
		if got := concurrentIndexCleanups[version]; got != index {
			t.Fatalf("concurrentIndexCleanups[%q] = %q, want %q", version, got, index)
		}
	}
	for _, direction := range []string{"up", "up", "down"} {
		var files []string
		for _, version := range versions {
			matches, err := filepath.Glob(filepath.Join("..", "..", "migrations", version+"."+direction+".sql"))
			if err != nil || len(matches) != 1 {
				t.Fatalf("migration file %s.%s.sql: %v %v", version, direction, matches, err)
			}
			files = append(files, matches...)
		}
		if err := runMigrations(ctx, pool, runOptions{
			Direction: direction, Files: files,
			SchemaMigrationsTable: schema + ".schema_migrations",
			AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
			Hooks:                 hooksForDirection(direction),
		}); err != nil {
			t.Fatal(err)
		}
		want := direction == "up"
		for _, index := range indexes {
			assertIndexExists(t, pool, schema, index, want)
		}
		for _, table := range []string{"issue_environment", "site_counter"} {
			var exists bool
			if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, schema+"."+table).Scan(&exists); err != nil || exists != want {
				t.Fatalf("%s after %s: exists=%v err=%v, want %v", table, direction, exists, err, want)
			}
		}
	}
}
