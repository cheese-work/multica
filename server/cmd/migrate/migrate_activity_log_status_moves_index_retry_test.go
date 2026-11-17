package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// CHE-6887: idx_activity_log_status_moves is built CONCURRENTLY IF NOT EXISTS on
// the largest append-only table. An interrupted build leaves an INVALID index of
// that name, IF NOT EXISTS skips the rebuild, and the runner records the
// migration as applied while the status-change queries stay on the slow walk.
// This runs the real migration file and the real registered hook against an
// INVALID leftover.
func TestRunMigrationsRepairsInvalidActivityLogStatusMovesIndex(t *testing.T) {
	t.Parallel()
	admin := openTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d_%d", time.Now().UnixNano(), rand.Uint32())
	schema := "migrate_status_moves_" + suffix
	schemaIdent := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schemaIdent); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA IF EXISTS "+schemaIdent+" CASCADE"); err != nil {
			t.Logf("drop schema %s: %v", schema, err)
		}
	})
	// Unqualified names in the migration and in the registered hook resolve
	// inside the test schema.
	pool := openTestPoolWithSearchPath(t, schema)

	const indexName = "idx_activity_log_status_moves"
	const version = "9999969_activity_log_status_moves_idx"
	files := realMigrationFiles(t, []string{version}, "up")
	body, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	createIndex := strings.TrimSpace(string(stripSQLLineComments(body)))
	if !strings.Contains(createIndex, indexName) || !strings.HasPrefix(strings.ToUpper(createIndex), "CREATE INDEX CONCURRENTLY IF NOT EXISTS") {
		t.Fatalf("migration %s does not build %s with CREATE INDEX CONCURRENTLY IF NOT EXISTS:\n%s", version, indexName, createIndex)
	}

	if _, err := pool.Exec(ctx, `CREATE TABLE activity_log (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		issue_id UUID,
		action TEXT NOT NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`); err != nil {
		t.Fatalf("create activity_log stand-in: %v", err)
	}

	// Interrupt the build the way a real one gets interrupted: an open
	// transaction that has written to the table owns an xid the concurrent
	// build must wait for, and statement_timeout cancels the wait.
	blocker, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire blocker conn: %v", err)
	}
	blockerTx, err := blocker.Begin(ctx)
	if err != nil {
		blocker.Release()
		t.Fatalf("begin blocker tx: %v", err)
	}
	if _, err := blockerTx.Exec(ctx, "INSERT INTO activity_log (issue_id, action) VALUES (gen_random_uuid(), 'status_changed')"); err != nil {
		blocker.Release()
		t.Fatalf("blocker insert: %v", err)
	}
	builder, err := pool.Acquire(ctx)
	if err != nil {
		blocker.Release()
		t.Fatalf("acquire builder conn: %v", err)
	}
	if _, err := builder.Exec(ctx, "SET statement_timeout = '2s'"); err != nil {
		builder.Release()
		blocker.Release()
		t.Fatalf("set statement_timeout: %v", err)
	}
	_, buildErr := builder.Exec(ctx, createIndex)
	// pgxpool does not reset session state, so clear the fuse before the
	// connection goes back to the pool.
	if _, err := builder.Exec(ctx, "SET statement_timeout = DEFAULT"); err != nil {
		t.Logf("reset statement_timeout: %v", err)
	}
	builder.Release()
	if buildErr == nil {
		blocker.Release()
		t.Fatal("interrupted build unexpectedly succeeded")
	}
	_ = blockerTx.Rollback(ctx)
	blocker.Release()
	assertIndexValidity(t, pool, schema, indexName, false)

	hook := preMigrationHooks[version]
	if hook == nil {
		t.Fatalf("production hook is not registered for %s", version)
	}
	if err := runMigrations(ctx, pool, runOptions{
		Direction:             "up",
		Files:                 files,
		SchemaMigrationsTable: schema + ".schema_migrations",
		AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
		Hooks:                 map[string]preMigrationHook{version: hook},
	}); err != nil {
		t.Fatalf("migrate up over an INVALID leftover: %v", err)
	}
	assertIndexValidity(t, pool, schema, indexName, true)
	if filepath.Base(files[0]) != version+".up.sql" {
		t.Fatalf("unexpected migration file %s", files[0])
	}
}
