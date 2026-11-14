package runinput

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL required")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func downSQL(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../migrations/999e_run_input_owner.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// 12c: each item of the gate is checked on its own.
func TestSchemaProblemsNamesEachMissingItem(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if probs, err := SchemaProblems(ctx, pool); err != nil || len(probs) != 0 {
		t.Fatalf("migrated schema: problems %v, err %v; want none", probs, err)
	}
	cases := []struct{ item, ddl string }{
		{"owner_task_id", `ALTER TABLE run_input DROP COLUMN owner_task_id`},
		{"thread_key", `ALTER TABLE run_input DROP COLUMN thread_key`},
		{"run_input_owned_has_owner", `ALTER TABLE run_input DROP CONSTRAINT run_input_owned_has_owner`},
		{"run_input_retire_held", `DROP TRIGGER run_input_retire_held ON run_input`},
		{"run_input_retire_held", `ALTER TABLE run_input DISABLE TRIGGER run_input_retire_held`},
		{"run_input_state_check", `ALTER TABLE run_input DROP CONSTRAINT run_input_state_check;
			ALTER TABLE run_input ADD CONSTRAINT run_input_state_check CHECK (state IN ('held','claimed','delivered','parked','discarded')) NOT VALID`},
	}
	for _, c := range cases {
		item, ddl := c.item, c.ddl
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, ddl); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("%s: %v", item, err)
		}
		probs, err := SchemaProblems(ctx, tx)
		_ = tx.Rollback(ctx)
		if err != nil {
			t.Fatalf("%s: %v", item, err)
		}
		if !strings.Contains(strings.Join(probs, ";"), item) {
			t.Errorf("without %q the gate reported %v", item, probs)
		}
	}
}

func TestRequireSchemaFailsAfterWindowWhenItemMissing(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DROP TRIGGER run_input_retire_held ON run_input`); err != nil {
		t.Fatal(err)
	}
	if err := RequireSchema(ctx, tx, 10*time.Millisecond, 50*time.Millisecond); err == nil {
		t.Fatal("RequireSchema passed on a schema without the retire trigger")
	}
}

// 13a, 13b: a held lease stops the down migration with no change; a released
// lease lets it run.
func TestLeaseBlocksDownMigration(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	lease, err := HoldLease(ctx, pool, time.Second, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, downSQL(t))
	_ = tx.Rollback(ctx)
	if err == nil || !strings.Contains(err.Error(), "new servers still running") {
		t.Fatalf("down migration with a held lease: err = %v, want 'new servers still running'", err)
	}
	if probs, err := SchemaProblems(ctx, pool); err != nil || len(probs) != 0 {
		t.Fatalf("schema changed by a refused down migration: %v %v", probs, err)
	}

	lease.Release()
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, downSQL(t)); err != nil {
		t.Fatalf("down migration after release: %v", err)
	}
	if probs, _ := SchemaProblems(ctx, tx); len(probs) == 0 {
		t.Fatal("after the down migration the gate still passes")
	}
}

// 13c: the guard notices a killed lease connection.
func TestLeaseGuardReportsLostConnection(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	var lost atomic.Bool
	lease, err := HoldLease(ctx, pool, 100*time.Millisecond, func(error) { lost.Store(true) })
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if _, err := pool.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_locks
		WHERE locktype = 'advisory' AND classid = $1 AND objid = $2 AND mode = 'ShareLock' AND granted`, LeaseClass, LeaseObj); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !lost.Load() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !lost.Load() {
		t.Fatal("onLost not called within 3 s of the lease connection being killed")
	}
}

var _ = pgx.ErrNoRows
