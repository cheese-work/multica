package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/runinput"
)

// Tests 11e, 12a, 12b, 13a to 13f of the CHE-5266 slice 2 design: the up and
// down boundary of migration 999e against frozen slice 1 SQL and the lease of
// internal/runinput. The lease key is database-wide, so these tests hold it only
// for the span they need.

const (
	oldClaimSQL  = `UPDATE run_input SET state = 'claimed', task_id = $3, updated_at = clock_timestamp() WHERE issue_id = $1 AND agent_id = $2 AND state = 'held' RETURNING event_id`
	oldInsertSQL = `INSERT INTO run_input (workspace_id, issue_id, agent_id, event_id, kind, payload) VALUES ($1, $2, $3, $4, 'comment', '{}') ON CONFLICT (issue_id, agent_id, event_id) DO NOTHING RETURNING state`
)

// oldClaim runs the slice 1 claim of (issue, agent) for task and returns the events it bound.
func (f *ownerFixture) oldClaim(task string) []string {
	f.t.Helper()
	rows, err := f.pool.Query(f.ctx, oldClaimSQL, f.issue, f.agent, task)
	if err != nil {
		f.t.Fatalf("old claim: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var ev string
		if err := rows.Scan(&ev); err != nil {
			f.t.Fatal(err)
		}
		got = append(got, ev)
	}
	return got
}

func (f *ownerFixture) oldInsertState(event string) string {
	f.t.Helper()
	var st string
	if err := f.pool.QueryRow(f.ctx, oldInsertSQL, f.ws, f.issue, f.agent, event).Scan(&st); err != nil {
		f.t.Fatalf("old insert %s: %v", event, err)
	}
	return st
}

// catalog is a digest of what the down migration must not change when refused.
func (f *ownerFixture) catalog() string {
	f.t.Helper()
	var s string
	if err := f.pool.QueryRow(f.ctx, `SELECT md5(
		(SELECT COALESCE(string_agg(conname::text || pg_get_constraintdef(oid), '|' ORDER BY conname), '') FROM pg_constraint WHERE conrelid = 'run_input'::regclass)
		|| (SELECT COALESCE(string_agg(tgname::text || tgenabled::text, '|' ORDER BY tgname), '') FROM pg_trigger WHERE tgrelid = 'run_input'::regclass AND NOT tgisinternal)
		|| (SELECT string_agg(attname::text, '|' ORDER BY attname) FROM pg_attribute WHERE attrelid = 'run_input'::regclass AND attnum > 0 AND NOT attisdropped))`).Scan(&s); err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *ownerFixture) lease(onLost func(error)) *runinput.Lease {
	f.t.Helper()
	l, err := runinput.HoldLease(f.ctx, f.pool, 100*time.Millisecond, onLost)
	if err != nil {
		f.t.Fatalf("hold lease: %v", err)
	}
	f.t.Cleanup(l.Release)
	return l
}

func (f *ownerFixture) gateFails(what string) {
	f.t.Helper()
	if err := runinput.RequireSchema(f.ctx, f.pool, 10*time.Millisecond, 60*time.Millisecond); err == nil {
		f.t.Fatalf("%s: the schema gate passed", what)
	}
}

// 12a: against a slice 1 schema the gate refuses, reads only, and leaves an old
// connection untouched; no lease is left behind.
func TestRunInputOwnerUpBoundaryGateRefusesUnmigratedSchema(t *testing.T) {
	f := newOwnerFixture(t)
	f.seedLegacyHeld()
	before := f.checksum()
	f.gateFails("slice 1 schema")
	if _, err := runinput.HoldLease(f.ctx, f.pool, time.Second, func(error) {}); err == nil {
		t.Fatal("HoldLease succeeded on a slice 1 schema")
	}
	if got := f.checksum(); got != before {
		t.Fatalf("the gate changed run_input: %s != %s", got, before)
	}
	if st := f.oldInsertState("old-loop"); st != "held" {
		t.Fatalf("old insert on the slice 1 schema stored %q, want held (old server keeps working)", st)
	}
	var free bool
	if err := f.pool.QueryRow(f.ctx, `SELECT pg_try_advisory_lock(5266, 1)`).Scan(&free); err != nil || !free {
		t.Fatalf("exclusive lease key free = %v (err %v): the refused HoldLease left its lock", free, err)
	}
	f.pool.Exec(f.ctx, `SELECT pg_advisory_unlock(5266, 1)`)
}

// 11e and 12b: the migration waits for an old claim in flight and then converts
// by evidence; the gate waits for the commit; old claims after it return nothing
// and old inserts park.
func TestRunInputOwnerUpBoundaryWaitsForOldClaimInFlight(t *testing.T) {
	f := newOwnerFixture(t)
	x, _, _ := f.seedLegacyHeld()
	old, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := old.Query(f.ctx, oldClaimSQL, f.issue, f.agent, x)
	if err != nil {
		t.Fatal(err)
	}
	var claimed int
	for rows.Next() {
		claimed++
	}
	rows.Close()
	if claimed != 8 {
		t.Fatalf("old claim in flight bound %d rows, want all 8 held rows", claimed)
	}
	migrated := make(chan error, 1)
	go func() { migrated <- f.migrate("up", "999e_run_input_owner") }()
	f.waitForLockWait()
	gate := make(chan error, 1)
	go func() { gate <- runinput.RequireSchema(f.ctx, f.pool, 20*time.Millisecond, 20*time.Second) }()
	select {
	case err := <-gate:
		t.Fatalf("the gate returned (%v) while the migration waited for the table lock", err)
	case err := <-migrated:
		t.Fatalf("the migration finished (%v) while the old claim held its rows", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := old.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-migrated; err != nil {
		t.Fatalf("migration: %v", err)
	}
	if err := <-gate; err != nil {
		t.Fatalf("gate after the commit: %v", err)
	}
	// The claim of x stands only for the row whose payload names x.
	f.want("a", "claimed", "NULL", "NULL", f.c1)
	f.wantQuarantined("b", "c", "d", "e", "f", "g", "h")
	if got := f.oldClaim(x); len(got) != 0 {
		t.Fatalf("old claim after the migration bound %v, want nothing", got)
	}
	if st := f.oldInsertState("after"); st != "parked" {
		t.Fatalf("old insert after the migration stored %q, want parked", st)
	}
	if r := f.row("after"); r.reason != "legacy_unowned" || r.owner != "NULL" {
		t.Fatalf("old insert row = %+v, want parked/legacy_unowned with no owner", r)
	}
	f.stateCounts()
}

// 13a, 13b, 13f: a held lease refuses the down migration with no change; after
// the release it runs; a straggler new-code write fails and old SQL works; up
// again loses no row and leaves none held.
func TestRunInputOwnerDownBoundaryLeaseThenRelease(t *testing.T) {
	f := newOwnerFixture(t)
	f.seedLegacyHeld()
	seeded := f.total()
	f.up()
	lease := f.lease(func(error) {})
	rows, cat := f.checksum(), f.catalog()

	err := f.migrate("down", "999e_run_input_owner")
	if err == nil || !strings.Contains(err.Error(), "new servers still running") {
		t.Fatalf("down with a live lease: err = %v, want 'new servers still running'", err)
	}
	if f.checksum() != rows || f.catalog() != cat {
		t.Fatal("a refused down migration changed rows or catalog")
	}
	if probs, err := runinput.SchemaProblems(f.ctx, f.pool); err != nil || len(probs) != 0 {
		t.Fatalf("gate after a refused down: %v %v", probs, err)
	}
	if st := f.oldInsertState("old-while-leased"); st != "parked" {
		t.Fatalf("old insert while the lease is held: %q, want parked", st)
	}
	f.exec(`UPDATE run_input SET state = 'owned', owner_task_id = $1, thread_key = $2 WHERE event_id = 'c'`, f.carrier("queued", &f.c2), f.c2)

	lease.Release()
	if err := f.migrate("down", "999e_run_input_owner"); err != nil {
		t.Fatalf("down after release: %v", err)
	}
	f.gateFails("after the down migration")
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO run_input (workspace_id, issue_id, agent_id, event_id, kind, state, owner_task_id)
		VALUES ($1, $2, $3, 'straggler', 'comment', 'owned', $4)`, f.ws, f.issue, f.agent, f.id()); err == nil {
		t.Fatal("a new-code owned insert succeeded after the down migration")
	}
	var n int
	f.pool.QueryRow(f.ctx, `SELECT count(*) FROM run_input WHERE event_id = 'straggler'`).Scan(&n)
	if n != 0 {
		t.Fatal("the failed straggler insert left a row")
	}
	if st := f.oldInsertState("old-after-down"); st != "held" {
		t.Fatalf("old insert after the down migration: %q, want held (old SQL works)", st)
	}
	f.up()
	var held int
	f.pool.QueryRow(f.ctx, `SELECT count(*) FROM run_input WHERE state = 'held'`).Scan(&held)
	if held != 0 || f.total() != seeded+2 {
		t.Fatalf("after up, down, up: held=%d total=%d, want 0 and %d (no row lost)", held, f.total(), seeded+2)
	}
}

// 13c and 13d: a killed lease connection is noticed within 10 s; the down
// migration then runs at once, a straggler new-code write fails loudly, nothing
// is delivered, and a restart is refused by the gate. The guard callback stands
// in for the process exit of cmd/server.
func TestRunInputOwnerDownBoundaryKilledLeaseGap(t *testing.T) {
	f := newOwnerFixture(t)
	f.seedLegacyHeld()
	f.up()
	var lost atomic.Bool
	f.lease(func(error) { lost.Store(true) })
	if _, err := f.pool.Exec(f.ctx, `SELECT pg_terminate_backend(pid) FROM pg_locks
		WHERE locktype = 'advisory' AND classid = 5266 AND objid = 1 AND mode = 'ShareLock' AND granted`); err != nil {
		t.Fatal(err)
	}
	// 13d: the gap, before the guard fires.
	if err := f.migrate("down", "999e_run_input_owner"); err != nil {
		t.Fatalf("down right after the kill: %v", err)
	}
	var delivered int
	f.pool.QueryRow(f.ctx, `SELECT count(*) FROM run_input WHERE state = 'delivered'`).Scan(&delivered)
	if _, err := f.pool.Exec(f.ctx, `UPDATE run_input SET state = 'claimed', owner_task_id = NULL, thread_key = NULL WHERE event_id = 'c'`); err == nil {
		t.Fatal("a new-code write succeeded after the down migration")
	}
	var after int
	f.pool.QueryRow(f.ctx, `SELECT count(*) FROM run_input WHERE state = 'delivered'`).Scan(&after)
	if after != delivered {
		t.Fatalf("delivered rows %d -> %d: an input was delivered through the gap", delivered, after)
	}
	// 13c: the guard reports the loss within 10 s; the restart is refused.
	deadline := time.Now().Add(10 * time.Second)
	for !lost.Load() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !lost.Load() {
		t.Fatal("the lease guard did not report the killed connection within 10 s")
	}
	f.gateFails("restart after the down migration")
}

// 13e: a failure in the middle of the down migration rolls back every change
// and releases the exclusive locks.
func TestRunInputOwnerDownBoundaryInjectedFailureRollsBack(t *testing.T) {
	f := newOwnerFixture(t)
	f.seedLegacyHeld()
	f.up()
	rows, cat := f.checksum(), f.catalog()
	b, err := readMigration("999e_run_input_owner.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	broken := strings.Replace(b, "ALTER TABLE run_input DROP CONSTRAINT IF EXISTS run_input_seq_key;", "SELECT 1/0;", 1)
	if broken == b {
		t.Fatal("injection point not found in the down migration")
	}
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(f.ctx, broken); err == nil {
		t.Fatal("the injected failure did not fail the down migration")
	}
	_ = tx.Rollback(f.ctx)
	if f.checksum() != rows || f.catalog() != cat {
		t.Fatal("rows or catalog changed after the failed down migration")
	}
	if probs, err := runinput.SchemaProblems(f.ctx, f.pool); err != nil || len(probs) != 0 {
		t.Fatalf("gate after the failed down: %v %v", probs, err)
	}
	other, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Rollback(f.ctx)
	var free bool
	if err := other.QueryRow(f.ctx, `SELECT pg_try_advisory_xact_lock(5266, 1)`).Scan(&free); err != nil || !free {
		t.Fatalf("barrier lock free = %v (err %v) after the rollback", free, err)
	}
	if _, err := other.Exec(f.ctx, `SET LOCAL lock_timeout = '500ms'; LOCK TABLE run_input IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("table lock after the rollback: %v", err)
	}
}

func readMigration(name string) (string, error) {
	b, err := os.ReadFile(filepath.Join("..", "..", "migrations", name))
	return string(b), err
}
