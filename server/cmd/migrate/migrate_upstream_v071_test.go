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

func TestUpstreamV071Migrations(t *testing.T) {
	admin := openTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	schema := fmt.Sprintf("migrate_v071_%d_%d", time.Now().UnixNano(), rand.Uint32())
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
	for _, statement := range []string{
		`CREATE TABLE runtime_profile (protocol_family TEXT CONSTRAINT runtime_profile_protocol_family_check CHECK (protocol_family IN ('codex')))`,
		`INSERT INTO runtime_profile VALUES ('codex')`,
		`CREATE TABLE agent_task_queue (id UUID, agent_id UUID, runtime_id UUID, completed_at TIMESTAMPTZ, failure_reason TEXT, status TEXT, result JSONB)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	versions := []string{"737_runtime_profile_add_devin", "738_agent_task_process_cleanup_index"}
	for _, direction := range []string{"up", "up", "down"} {
		var files []string
		for _, version := range versions {
			matches, err := filepath.Glob(filepath.Join("..", "..", "migrations", version+"."+direction+".sql"))
			if err != nil {
				t.Fatal(err)
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
		if direction == "up" {
			t.Run("devin_profile_accepted", func(t *testing.T) {
				if _, err := pool.Exec(ctx, "INSERT INTO runtime_profile VALUES ('devin')"); err != nil {
					t.Fatalf("Devin runtime profile must be accepted after upgrade: %v", err)
				}
			})
			t.Run("cleanup_index_valid", func(t *testing.T) {
				assertIndexExists(t, pool, schema, "idx_agent_task_queue_process_cleanup", true)
				if got := concurrentIndexCleanups[versions[1]]; got != "idx_agent_task_queue_process_cleanup" {
					t.Fatalf("renumbered migration cleanup hook = %q", got)
				}
			})
			continue
		}
		assertIndexExists(t, pool, schema, "idx_agent_task_queue_process_cleanup", false)
		if _, err := pool.Exec(ctx, "INSERT INTO runtime_profile VALUES ('devin')"); err == nil {
			t.Fatal("rollback must reject new Devin profiles")
		}
		var retained int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM runtime_profile").Scan(&retained); err != nil || retained != 3 {
			t.Fatalf("rollback must preserve existing profiles: count=%d, error=%v", retained, err)
		}
	}
}
