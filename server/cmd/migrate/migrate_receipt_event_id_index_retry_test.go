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

// CHE-6889: an interrupted build of the receipt event_id index leaves an INVALID
// index of the same name; `IF NOT EXISTS` would skip the rebuild and
// MarkStartedPREventsDelivered would stay on a full scan of
// issue_wakeup_receipt. The registered hook drops the leftover, the retry
// builds the index, and it is valid.
func TestRunMigrationsRepairsInvalidReceiptEventIDIndexBeforeRetry(t *testing.T) {
	t.Parallel()
	adminPool := openTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d_%d", time.Now().UnixNano(), rand.Uint32())
	schema := "migrate_receipt_event_id_" + suffix
	schemaIdent := pgx.Identifier{schema}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE SCHEMA "+schemaIdent); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if _, err := adminPool.Exec(cleanupCtx, "DROP SCHEMA IF EXISTS "+schemaIdent+" CASCADE"); err != nil {
			t.Logf("drop schema %s: %v", schema, err)
		}
	})
	pool := openTestPoolWithSearchPath(t, schema)

	const version = "999l_issue_wakeup_receipt_pr_event_id_idx"
	const indexName = "idx_issue_wakeup_receipt_pr_event_id"
	migrationPath := filepath.Join("..", "..", "migrations", version+".up.sql")
	sqlBytes, err := os.ReadFile(migrationPath)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	createIndex := strings.TrimSuffix(strings.TrimSpace(string(sqlBytes)), ";")
	if !strings.Contains(createIndex, "CREATE INDEX CONCURRENTLY IF NOT EXISTS "+indexName) {
		t.Fatalf("migration does not build %s concurrently with IF NOT EXISTS:\n%s", indexName, createIndex)
	}
	if preMigrationHooks[version] == nil {
		t.Fatalf("production hook is not registered for %s", version)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE issue_wakeup_receipt (wakeup_id UUID NOT NULL, event_type TEXT NOT NULL, payload JSONB NOT NULL)`); err != nil {
		t.Fatalf("create receipt table: %v", err)
	}

	// Interrupt the build the way a real one is interrupted: an open transaction
	// that has written to the table owns an xid the concurrent build waits for,
	// and statement_timeout cancels the wait.
	blocker, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire blocker conn: %v", err)
	}
	blockerTx, err := blocker.Begin(ctx)
	if err != nil {
		blocker.Release()
		t.Fatalf("begin blocker tx: %v", err)
	}
	if _, err := blockerTx.Exec(ctx, `INSERT INTO issue_wakeup_receipt VALUES (gen_random_uuid(), 'pr.event', '{"event_id":"e"}')`); err != nil {
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
	if _, err := builder.Exec(ctx, "SET statement_timeout = DEFAULT"); err != nil {
		t.Logf("reset statement_timeout: %v", err)
	}
	builder.Release()
	_ = blockerTx.Rollback(ctx)
	blocker.Release()
	if buildErr == nil {
		t.Fatal("interrupted build unexpectedly succeeded")
	}
	assertIndexValidity(t, pool, schema, indexName, false)

	options := runOptions{
		Direction:             "up",
		Files:                 []string{migrationPath},
		SchemaMigrationsTable: "schema_migrations",
		AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
		Hooks:                 map[string]preMigrationHook{version: preMigrationHooks[version]},
	}
	if err := runMigrations(ctx, pool, options); err != nil {
		t.Fatalf("retry migration after invalid concurrent index: %v", err)
	}
	assertIndexValidity(t, pool, schema, indexName, true)
}
