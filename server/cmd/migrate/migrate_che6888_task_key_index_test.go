package main

import (
	"context"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// CHE-6888: ListGitHubReviewAuthorCandidates matches a task by the expression
// right(replace(id::text, '-', ”), 12). Migration 999l indexes it. An
// interrupted CONCURRENTLY build leaves an INVALID index of the same name that
// `IF NOT EXISTS` skips on retry, so the migration is recorded as applied while
// the query stays on a full scan of agent_task_queue. The registered hook must
// drop the leftover; this runs the real migration file and the real hooks.
func TestRunMigrationsRepairsInvalidTaskKeyIndex(t *testing.T) {
	t.Parallel()
	adminPool := openTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	schema := createScratchSchema(t, ctx, adminPool, "migrate_che6888_")
	pool := openTestPoolWithSearchPath(t, schema)
	const indexName = "idx_agent_task_queue_task_key"
	const version = "999l_agent_task_queue_task_key_idx"

	if _, err := pool.Exec(ctx, `CREATE TABLE agent_task_queue (id UUID PRIMARY KEY DEFAULT gen_random_uuid())`); err != nil {
		t.Fatalf("create task table: %v", err)
	}
	// Interrupt a build the way a real one is interrupted: an open transaction
	// that wrote to the table owns an xid the build waits for, and
	// statement_timeout cancels the wait.
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker tx: %v", err)
	}
	if _, err := blocker.Exec(ctx, `INSERT INTO agent_task_queue DEFAULT VALUES`); err != nil {
		t.Fatalf("blocker insert: %v", err)
	}
	builder, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire builder conn: %v", err)
	}
	if _, err := builder.Exec(ctx, "SET statement_timeout = '2s'"); err != nil {
		t.Fatalf("set statement_timeout: %v", err)
	}
	_, buildErr := builder.Exec(ctx, `CREATE INDEX CONCURRENTLY IF NOT EXISTS `+pgx.Identifier{indexName}.Sanitize()+
		` ON agent_task_queue ((right(replace(id::text, '-', ''), 12)))`)
	if _, err := builder.Exec(ctx, "SET statement_timeout = DEFAULT"); err != nil {
		t.Logf("reset statement_timeout: %v", err)
	}
	builder.Release()
	_ = blocker.Rollback(ctx)
	if buildErr == nil {
		t.Fatal("interrupted build unexpectedly succeeded")
	}
	assertIndexValidity(t, pool, schema, indexName, false)

	if _, err := pool.Exec(ctx, `CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	opts := runOptions{
		Direction:             "up",
		Files:                 realMigrationFiles(t, []string{version}, "up"),
		SchemaMigrationsTable: schema + ".schema_migrations",
		AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
		Hooks:                 hooksForDirection("up"),
	}
	if opts.Hooks[version] == nil {
		t.Fatalf("production hook is not registered for %s", version)
	}
	if err := runMigrations(ctx, pool, opts); err != nil {
		t.Fatalf("migrate up over an invalid %s: %v", indexName, err)
	}
	assertIndexValidity(t, pool, schema, indexName, true)
}
