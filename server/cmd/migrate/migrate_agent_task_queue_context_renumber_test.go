package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestAgentTaskQueueContextRenumberKeepsTheColumns(t *testing.T) {
	t.Parallel()
	const version = "630_agent_task_queue_context"
	const legacyVersion = "627_agent_task_queue_context"

	for _, previouslyApplied := range []bool{false, true} {
		t.Run(fmt.Sprintf("previously_applied_%t", previouslyApplied), func(t *testing.T) {
			t.Parallel()
			admin := openTestPool(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			schema := fmt.Sprintf("migrate_task_context_%d_%d", time.Now().UnixNano(), rand.Uint32())
			ident := pgx.Identifier{schema}.Sanitize()
			if _, err := admin.Exec(ctx, "CREATE SCHEMA "+ident); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+ident+" CASCADE"); err != nil {
					t.Errorf("cleanup schema: %v", err)
				}
			})
			pool := openTestPoolWithSearchPath(t, schema)
			if _, err := pool.Exec(ctx, `CREATE TABLE schema_migrations (
				version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ DEFAULT now()
			)`); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `CREATE TABLE agent_task_queue (id UUID PRIMARY KEY)`); err != nil {
				t.Fatal(err)
			}
			files := realMigrationFiles(t, []string{version}, "up")
			if previouslyApplied {
				sql, err := os.ReadFile(files[0])
				if err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(ctx, string(sql)); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(ctx, "INSERT INTO agent_task_queue (id) VALUES (gen_random_uuid())"); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(ctx, "UPDATE agent_task_queue SET context_tokens = 123, context_window = 456"); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", legacyVersion); err != nil {
					t.Fatal(err)
				}
			}
			options := runOptions{
				Direction: "up", Files: files,
				SchemaMigrationsTable: schema + ".schema_migrations",
				AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
				Hooks:                 hooksForDirection("up"),
			}
			for range 2 {
				if err := runMigrations(ctx, pool, options); err != nil {
					t.Fatal(err)
				}
			}
			var recorded bool
			if err := pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", version).Scan(&recorded); err != nil || !recorded {
				t.Fatalf("new version recorded = %t, error = %v", recorded, err)
			}
			var columns int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
				WHERE table_schema = $1 AND table_name = 'agent_task_queue'
				  AND column_name IN ('context_tokens', 'context_window') AND data_type = 'bigint'`, schema).Scan(&columns); err != nil || columns != 2 {
				t.Fatalf("bigint context columns = %d, error = %v", columns, err)
			}
			if previouslyApplied {
				var tokens, window int64
				if err := pool.QueryRow(ctx, "SELECT context_tokens, context_window FROM agent_task_queue").Scan(&tokens, &window); err != nil || tokens != 123 || window != 456 {
					t.Fatalf("reapplied migration changed stored values: %d %d, error = %v", tokens, window, err)
				}
				if err := pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", legacyVersion).Scan(&recorded); err != nil || !recorded {
					t.Fatalf("legacy version preserved = %t, error = %v", recorded, err)
				}
			}
		})
	}
}
