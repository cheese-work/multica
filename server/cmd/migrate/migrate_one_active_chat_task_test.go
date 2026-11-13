package main

import (
	"context"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	oneActiveChatTaskVersion = "962_agent_task_queue_one_active_per_chat"
	oneActiveChatTaskIndex   = "idx_agent_task_queue_one_active_per_chat"
)

func oneActiveChatTaskScratch(t *testing.T, ctx context.Context) (*pgxpool.Pool, string) {
	t.Helper()
	schema := createScratchSchema(t, ctx, openTestPool(t), "migrate_one_active_chat_")
	pool := openTestPoolWithSearchPath(t, schema)
	if _, err := pool.Exec(ctx, `CREATE TABLE agent_task_queue (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(), chat_session_id UUID, status TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
  )`); err != nil {
		t.Fatal(err)
	}
	return pool, schema
}

func oneActiveChatTaskOptions(schema, direction string) runOptions {
	return runOptions{
		Direction:             direction,
		Files:                 nil,
		SchemaMigrationsTable: schema + ".schema_migrations",
		AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
		Hooks:                 hooksForDirection(direction),
	}
}

func TestOneActiveChatTaskPreflightStopsOnDuplicates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, schema := oneActiveChatTaskScratch(t, ctx)
	for _, statement := range []string{
		`INSERT INTO agent_task_queue (chat_session_id, status) VALUES
      (md5('dm-dup')::uuid, 'running'), (md5('dm-dup')::uuid, 'dispatched'),
      (md5('dm-ok')::uuid, 'running'), (md5('dm-ok')::uuid, 'queued'), (md5('dm-ok')::uuid, 'completed')`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	options := oneActiveChatTaskOptions(schema, "up")
	options.Files = realMigrationFiles(t, []string{oneActiveChatTaskVersion}, "up")
	err := runMigrations(ctx, pool, options)
	if err == nil {
		t.Fatal("migration must stop when a DM has more than one active task")
	}
	var dupID string
	if scanErr := pool.QueryRow(ctx, `SELECT md5('dm-dup')::uuid::text`).Scan(&dupID); scanErr != nil {
		t.Fatal(scanErr)
	}
	if !strings.Contains(err.Error(), dupID) {
		t.Fatalf("error must name the duplicate chat_session_id %s: %v", dupID, err)
	}
	var okID string
	if scanErr := pool.QueryRow(ctx, `SELECT md5('dm-ok')::uuid::text`).Scan(&okID); scanErr != nil {
		t.Fatal(scanErr)
	}
	if strings.Contains(err.Error(), okID) {
		t.Fatalf("error must not name a DM with one active task: %v", err)
	}
	assertMigrationVersionRecorded(t, ctx, pool, schema, oneActiveChatTaskVersion, false)
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue`).Scan(&rows); err != nil || rows != 5 {
		t.Fatalf("preflight must not clean up rows: count=%d err=%v", rows, err)
	}
	assertIndexExists(t, pool, schema, oneActiveChatTaskIndex, false)
}

func TestOneActiveChatTaskIndexRejectsSecondActiveRow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, schema := oneActiveChatTaskScratch(t, ctx)
	if _, err := pool.Exec(ctx, `INSERT INTO agent_task_queue (chat_session_id, status)
    VALUES (md5('dm-a')::uuid, 'completed'), (md5('dm-a')::uuid, 'queued')`); err != nil {
		t.Fatal(err)
	}
	options := oneActiveChatTaskOptions(schema, "up")
	options.Files = realMigrationFiles(t, []string{oneActiveChatTaskVersion}, "up")
	if err := runMigrations(ctx, pool, options); err != nil {
		t.Fatal(err)
	}
	assertIndexValidity(t, pool, schema, oneActiveChatTaskIndex, true)
	assertMigrationVersionRecorded(t, ctx, pool, schema, oneActiveChatTaskVersion, true)

	insert := func(dm, status string) error {
		_, err := pool.Exec(ctx, `INSERT INTO agent_task_queue (chat_session_id, status)
      VALUES (CASE WHEN $1 = '' THEN NULL ELSE md5($1)::uuid END, $2)`, dm, status)
		return err
	}
	for _, active := range []string{"dispatched", "waiting_local_directory", "running"} {
		if err := insert("dm-"+active, active); err != nil {
			t.Fatalf("first %s row: %v", active, err)
		}
		for _, second := range []string{"dispatched", "waiting_local_directory", "running"} {
			err := insert("dm-"+active, second)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != oneActiveChatTaskIndex {
				t.Fatalf("second %s after %s = %v, want 23505 on %s", second, active, err, oneActiveChatTaskIndex)
			}
		}
		// Queued rows, history, and other DMs stay allowed next to an active row.
		for _, ok := range [][2]string{{"dm-" + active, "queued"}, {"dm-" + active, "deferred"}, {"dm-" + active, "completed"}, {"dm-other-" + active, "running"}} {
			if err := insert(ok[0], ok[1]); err != nil {
				t.Fatalf("%v must be allowed next to active %s: %v", ok, active, err)
			}
		}
	}
	// Non-chat tasks (NULL chat_session_id) are not limited.
	for range 3 {
		if err := insert("", "running"); err != nil {
			t.Fatalf("NULL chat_session_id row must be allowed: %v", err)
		}
	}
	// A queued row can become active only if the DM has no active row.
	if err := insert("dm-claim", "queued"); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(ctx, `UPDATE agent_task_queue SET status = 'running' WHERE chat_session_id = md5('dm-running')::uuid AND status = 'queued'`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("queued→running on a DM with an active task = %v, want 23505", err)
	}

	options.Direction = "down"
	options.Files = realMigrationFiles(t, []string{oneActiveChatTaskVersion}, "down")
	options.Hooks = hooksForDirection("down")
	if err := runMigrations(ctx, pool, options); err != nil {
		t.Fatal(err)
	}
	assertIndexExists(t, pool, schema, oneActiveChatTaskIndex, false)
	assertMigrationVersionRecorded(t, ctx, pool, schema, oneActiveChatTaskVersion, false)
}
