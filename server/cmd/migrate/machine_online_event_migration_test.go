package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Install, trigger-off and re-enable files of the durable offline-period epoch
// (CHE-9254 stage 2, plan r3). Every test runs on a private schema that holds
// its own copy of agent_runtime, so a held lock never touches another test.
//
// The writer-pause budget is 1.5 s per attempt: lock_timeout caps the wait at
// 0.5 s and statement_timeout caps each statement under the lock at 0.6 s.

const (
	moeFileAUp     = "99k_machine_online_event.up.sql"
	moeFileADown   = "99k_machine_online_event.down.sql"
	moeFileBUp     = "99l_machine_online_event_trigger.up.sql"
	moeFileBDown   = "99l_machine_online_event_trigger.down.sql"
	moeTriggerOff  = "machine_online_event_trigger_off.sql"
	moeTriggerOn   = "machine_online_event_trigger_on.sql"
	moeWriterBound = 1500 * time.Millisecond
)

type moeEnv struct {
	pool   *pgxpool.Pool // search_path = the private schema
	admin  *pgxpool.Pool
	schema string
	rel    string // quoted agent_runtime of the private schema
}

func newMoeEnv(t *testing.T) *moeEnv {
	t.Helper()
	admin := openTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	schema := fmt.Sprintf("moe_migrate_%d_%d", time.Now().UnixNano(), rand.Uint32())
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+ident); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := admin.Exec(c, "DROP SCHEMA IF EXISTS "+ident+" CASCADE"); err != nil {
			t.Logf("drop schema %s: %v", schema, err)
		}
	})
	pool := openTestPoolWithSearchPath(t, schema)
	if _, err := pool.Exec(ctx, `CREATE TABLE agent_runtime (LIKE public.agent_runtime INCLUDING ALL)`); err != nil {
		t.Fatalf("create private agent_runtime (the migrated public schema must exist): %v", err)
	}
	return &moeEnv{pool: pool, admin: admin, schema: schema, rel: pgx.Identifier{schema, "agent_runtime"}.Sanitize()}
}

func moeRead(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func moeMigration(t *testing.T, name string) string {
	return moeRead(t, filepath.Join("..", "..", "migrations"), name)
}
func moeTestdata(t *testing.T, name string) string { return moeRead(t, "testdata", name) }

// apply runs a file the way the runner does: one conn.Exec of the whole text,
// so a multi-statement file is one implicit transaction.
func (e *moeEnv) apply(sql string) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()
	start := time.Now()
	_, err = conn.Exec(ctx, sql)
	return time.Since(start), err
}

func (e *moeEnv) mustApply(t *testing.T, name, sql string) {
	t.Helper()
	if _, err := e.apply(sql); err != nil {
		t.Fatalf("apply %s: %v", name, err)
	}
}

func (e *moeEnv) install(t *testing.T) {
	t.Helper()
	e.mustApply(t, moeFileAUp, moeMigration(t, moeFileAUp))
	e.mustApply(t, moeFileBUp, moeMigration(t, moeFileBUp))
}

func (e *moeEnv) runtime(t *testing.T, daemonID, status string) string {
	t.Helper()
	var id string
	err := e.pool.QueryRow(context.Background(),
		`INSERT INTO agent_runtime (workspace_id, name, runtime_mode, provider, status, daemon_id, owner_id)
		 VALUES (gen_random_uuid(), $1, 'local', 'moe', $2, $1, gen_random_uuid()) RETURNING id::text`, daemonID, status).Scan(&id)
	if err != nil {
		t.Fatalf("insert runtime %s: %v", daemonID, err)
	}
	return id
}

func (e *moeEnv) one(t *testing.T, query string, args ...any) string {
	t.Helper()
	var s *string
	if err := e.pool.QueryRow(context.Background(), query, args...).Scan(&s); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	if s == nil {
		return ""
	}
	return *s
}

func (e *moeEnv) generation(t *testing.T) string {
	return e.one(t, `SELECT generation::text FROM machine_online_event_generation`)
}

// triggers reads the trigger state of the private agent_runtime, name:enabled.
func (e *moeEnv) triggers(t *testing.T) string {
	return e.one(t, `SELECT coalesce(string_agg(tgname || ':' || tgenabled, ',' ORDER BY tgname), '')
		FROM pg_trigger WHERE tgrelid = $1::text::regclass AND NOT tgisinternal`, e.rel)
}

func (e *moeEnv) functions(t *testing.T) string {
	return e.one(t, `SELECT coalesce(string_agg(p.proname, ','), '') FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = $1 AND p.proname LIKE 'machine_online_event%'`, e.schema)
}

// ungranted counts lock requests that wait on the private agent_runtime.
func (e *moeEnv) ungranted(t *testing.T) int {
	var n int
	if err := e.admin.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_locks WHERE relation = $1::text::regclass AND NOT granted`, e.rel).Scan(&n); err != nil {
		t.Fatalf("pg_locks: %v", err)
	}
	return n
}

func (e *moeEnv) grantedShareRowExclusive(t *testing.T) bool {
	var n int
	if err := e.admin.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_locks WHERE relation = $1::text::regclass AND granted AND mode = 'ShareRowExclusiveLock'`, e.rel).Scan(&n); err != nil {
		t.Fatalf("pg_locks: %v", err)
	}
	return n > 0
}

func moeWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type moeResult struct {
	dur time.Duration
	err error
}

// flip is a writer: one status flip on a dedicated connection, bounded the way
// the plan states (statement_timeout 1500 ms).
func (e *moeEnv) flip(runtimeID, status string) moeResult {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		return moeResult{err: err}
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET statement_timeout = '1500ms'`); err != nil {
		return moeResult{err: err}
	}
	defer conn.Exec(context.Background(), `RESET statement_timeout`)
	start := time.Now()
	_, err = conn.Exec(ctx, `UPDATE agent_runtime SET status = $2 WHERE id = $1`, runtimeID, status)
	return moeResult{dur: time.Since(start), err: err}
}

// holdAndRun holds ROW EXCLUSIVE on agent_runtime, starts the file, queues a
// writer behind the file's lock request, then commits the holder. It returns
// the file result, the file's end time since the holder committed, and the
// writer result. The holder commit is the moment the file's lock is granted.
func (e *moeEnv) holdAndRun(t *testing.T, sql string, writerRuntime string) (file moeResult, fileAfterLock time.Duration, writer moeResult) {
	t.Helper()
	ctx := context.Background()
	holderID := e.runtime(t, "moe-holder-"+writerRuntime[:8], "offline")
	holder, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire holder: %v", err)
	}
	defer holder.Release()
	if _, err := holder.Exec(ctx, `BEGIN`); err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	if _, err := holder.Exec(ctx, `UPDATE agent_runtime SET name = name WHERE id = $1`, holderID); err != nil {
		t.Fatalf("holder update: %v", err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	var fileEnd, writerEnd time.Time
	go func() {
		defer wg.Done()
		file.dur, file.err = e.apply(sql)
		fileEnd = time.Now()
	}()
	moeWaitFor(t, "the file to wait for its lock", func() bool { return e.ungranted(t) >= 1 })
	writerStart := time.Now()
	go func() {
		defer wg.Done()
		writer = e.flip(writerRuntime, "online")
		writerEnd = time.Now()
	}()
	moeWaitFor(t, "the writer to queue behind the file", func() bool { return e.ungranted(t) >= 2 })
	lockAt := time.Now()
	if _, err := holder.Exec(ctx, `COMMIT`); err != nil {
		t.Fatalf("holder commit: %v", err)
	}
	wg.Wait()
	writer.dur = writerEnd.Sub(writerStart)
	return file, fileEnd.Sub(lockAt), writer
}

func moeSQLState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// blockedFailure is M1: an open writer holds the table, so the file cannot get
// its lock. It must fail with lock_timeout in 0.4 to 1.5 s, and a writer that
// queued behind it must then finish within the budget.
func (e *moeEnv) blockedFailure(t *testing.T, label, sql string) {
	t.Helper()
	ctx := context.Background()
	holderID := e.runtime(t, "moe-bf-holder", "offline")
	writerRuntime := e.runtime(t, "moe-bf-writer", "offline")
	holder, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire holder: %v", err)
	}
	defer holder.Release()
	if _, err := holder.Exec(ctx, `BEGIN`); err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	defer holder.Exec(ctx, `ROLLBACK`)
	if _, err := holder.Exec(ctx, `UPDATE agent_runtime SET name = name WHERE id = $1`, holderID); err != nil {
		t.Fatalf("holder update: %v", err)
	}
	var file, writer moeResult
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); file.dur, file.err = e.apply(sql) }()
	moeWaitFor(t, label+" to wait for its lock", func() bool { return e.ungranted(t) >= 1 })
	writerStart := time.Now()
	var writerEnd time.Time
	go func() { defer wg.Done(); writer = e.flip(writerRuntime, "online"); writerEnd = time.Now() }()
	wg.Wait()
	if got := moeSQLState(file.err); got != "55P03" {
		t.Fatalf("%s while a writer is open: err = %v, want SQLSTATE 55P03", label, file.err)
	}
	if file.dur < 400*time.Millisecond || file.dur > moeWriterBound {
		t.Fatalf("%s failed after %v, want 0.4 to 1.5 s", label, file.dur)
	}
	if writer.err != nil {
		t.Fatalf("%s: queued writer failed: %v", label, writer.err)
	}
	if d := writerEnd.Sub(writerStart); d > moeWriterBound {
		t.Fatalf("%s: queued writer took %v, want within 1.5 s", label, d)
	}
	t.Logf("%s blocked: failed 55P03 after %v; queued writer finished in %v", label, file.dur, writerEnd.Sub(writerStart))
}

// repeatedRuns is M2: a writer flips a private runtime in a loop while the file
// runs n times. No writer error, and both maxima stay under the budget.
func (e *moeEnv) repeatedRuns(t *testing.T, label, sql string, n int) {
	t.Helper()
	runtimeID := e.runtime(t, "moe-rr-writer", "offline")
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var maxWriter time.Duration
	var writerErr error
	var flips int
	wg.Add(1)
	go func() {
		defer wg.Done()
		status := "online"
		for {
			select {
			case <-stop:
				return
			default:
			}
			r := e.flip(runtimeID, status)
			if r.err != nil {
				writerErr = r.err
				return
			}
			flips++
			if r.dur > maxWriter {
				maxWriter = r.dur
			}
			if status == "online" {
				status = "offline"
			} else {
				status = "online"
			}
		}
	}()
	var maxFile time.Duration
	var fileErr error
	for i := 0; i < n && fileErr == nil; i++ {
		var d time.Duration
		d, fileErr = e.apply(sql)
		if d > maxFile {
			maxFile = d
		}
	}
	close(stop)
	wg.Wait()
	if fileErr != nil {
		t.Fatalf("%s run failed: %v", label, fileErr)
	}
	if writerErr != nil {
		t.Fatalf("%s: a writer failed while the file ran: %v", label, writerErr)
	}
	if flips == 0 {
		t.Fatalf("%s: the writer made no flip", label)
	}
	if maxWriter >= moeWriterBound || maxFile >= moeWriterBound {
		t.Fatalf("%s: longest writer %v, longest file %v, want both under 1.5 s", label, maxWriter, maxFile)
	}
	t.Logf("%s x%d: %d writer flips, longest writer latency %v, longest file duration %v", label, n, flips, maxWriter, maxFile)
}

// M1: a timeout leaves nothing, and file A needs no lock on agent_runtime.
func TestMachineOnlineEventMigration_M1_TimeoutLeavesNothing(t *testing.T) {
	e := newMoeEnv(t)
	ctx := context.Background()
	fileA, fileB := moeMigration(t, moeFileAUp), moeMigration(t, moeFileBUp)

	// File A takes no lock on agent_runtime: it commits while the table is locked ACCESS EXCLUSIVE.
	holder, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := holder.Exec(ctx, `BEGIN`); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := holder.Exec(ctx, `LOCK TABLE agent_runtime IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock: %v", err)
	}
	d, err := e.apply(fileA)
	_, _ = holder.Exec(ctx, `ROLLBACK`)
	holder.Release()
	if err != nil || d > time.Second {
		t.Fatalf("file A under an ACCESS EXCLUSIVE lock on agent_runtime: %v after %v; want a prompt commit", err, d)
	}
	if got := e.one(t, `SELECT count(*)::text FROM machine_online_event_generation`); got != "1" {
		t.Fatalf("generation rows after file A = %s, want 1", got)
	}

	e.blockedFailure(t, "file B", fileB)
	if got := e.triggers(t); got != "" {
		t.Fatalf("triggers after the failed file B = %q, want none", got)
	}
	if got := e.functions(t); got != "" {
		t.Fatalf("functions after the failed file B = %q, want none", got)
	}
}

// M2: file B install, repeated, under a flipping writer, within the budget.
func TestMachineOnlineEventMigration_M2_WriterPauseBudget(t *testing.T) {
	e := newMoeEnv(t)
	e.mustApply(t, moeFileAUp, moeMigration(t, moeFileAUp))
	e.repeatedRuns(t, "file B", moeMigration(t, moeFileBUp), 20)
	if got := e.triggers(t); got != "agent_runtime_online_event_ins:O,agent_runtime_online_event_upd:O" {
		t.Fatalf("triggers after file B = %q, want both enabled", got)
	}
}

// M3: the trigger-off and re-enable files meet the M1 and M2 bounds; the
// re-enable file changes the generation and enables both triggers in one commit.
func TestMachineOnlineEventMigration_M3_TriggerOffAndOn(t *testing.T) {
	e := newMoeEnv(t)
	e.install(t)
	const enabled = "agent_runtime_online_event_ins:O,agent_runtime_online_event_upd:O"
	const disabled = "agent_runtime_online_event_ins:D,agent_runtime_online_event_upd:D"
	off, on := moeTestdata(t, moeTriggerOff), moeTestdata(t, moeTriggerOn)
	g0 := e.generation(t)

	e.blockedFailure(t, "trigger-off", off)
	if got := e.triggers(t); got != enabled {
		t.Fatalf("triggers after the failed trigger-off = %q, want %q", got, enabled)
	}
	e.repeatedRuns(t, "trigger-off", off, 20)
	if got := e.triggers(t); got != disabled {
		t.Fatalf("triggers after trigger-off = %q, want %q", got, disabled)
	}

	e.blockedFailure(t, "re-enable", on)
	if got, g := e.triggers(t), e.generation(t); got != disabled || g != g0 {
		t.Fatalf("after the failed re-enable: triggers %q, generation %s; want %q and unchanged %s", got, g, disabled, g0)
	}
	e.mustApply(t, moeTriggerOn, on)
	if got := e.triggers(t); got != enabled {
		t.Fatalf("triggers after re-enable = %q, want %q", got, enabled)
	}
	g1 := e.generation(t)
	if g1 == g0 {
		t.Fatalf("generation after re-enable = %s, same as before", g1)
	}
	// One commit: the generation row and both trigger rows carry the same xmin.
	xmins := e.one(t, `SELECT string_agg(DISTINCT x, ',') FROM (
			SELECT xmin::text AS x FROM machine_online_event_generation
			UNION ALL SELECT xmin::text FROM pg_trigger WHERE tgrelid = $1::text::regclass AND NOT tgisinternal) s`, e.rel)
	if strings.Contains(xmins, ",") {
		t.Fatalf("generation and triggers changed in different transactions (xmin %s)", xmins)
	}
	e.repeatedRuns(t, "re-enable", on, 20)
	if got := e.triggers(t); got != enabled {
		t.Fatalf("triggers after the repeated re-enable = %q, want %q", got, enabled)
	}
}

// M4: a schema reversal and re-install makes a new generation, so a machine
// with no events gets a new id.
func TestMachineOnlineEventMigration_M4_SchemaResetNewGeneration(t *testing.T) {
	e := newMoeEnv(t)
	e.install(t)
	e.runtime(t, "moe-m4-x", "offline")
	g0 := e.generation(t)
	if got := e.one(t, `SELECT count(*)::text FROM machine_online_event WHERE daemon_id = 'moe-m4-x'`); got != "0" {
		t.Fatalf("machine X has %s events before the reset, want 0", got)
	}

	e.mustApply(t, moeFileBDown, moeMigration(t, moeFileBDown))
	e.mustApply(t, moeFileADown, moeMigration(t, moeFileADown))
	if got := e.one(t, `SELECT to_regclass('machine_online_event')::text || to_regclass('machine_online_event_generation')::text`); got != "" {
		t.Fatalf("tables after the down files = %q, want none", got)
	}
	e.install(t)
	g1 := e.generation(t)
	if g1 == g0 {
		t.Fatalf("generation after the reset = %s, same as before: X's id e%s.0 would come back", g1, g1)
	}
	if got := e.one(t, `SELECT count(*)::text FROM machine_online_event WHERE daemon_id = 'moe-m4-x'`); got != "0" {
		t.Fatalf("machine X has %s events after the reset, want 0", got)
	}
}

// moeInjectDelay splits the file text after its last statement that takes the
// agent_runtime lock and adds a 2 s sleep there, where the lock is held.
func moeInjectDelay(t *testing.T, sql string) string {
	t.Helper()
	at := -1
	for _, marker := range []string{"CREATE OR REPLACE TRIGGER", "ALTER TABLE"} {
		if i := strings.LastIndex(sql, marker); i > at {
			at = i
		}
	}
	if at < 0 {
		t.Fatalf("no lock statement found in the file")
	}
	end := strings.Index(sql[at:], ";")
	if end < 0 {
		t.Fatalf("the last lock statement has no terminator")
	}
	end += at + 1
	return sql[:end] + "\nSELECT pg_sleep(2);" + sql[end:]
}

// FP-02: a writer queued behind each file finishes within 1.5 s, as written
// and with a delay injected under the lock; with the delay the file rolls back.
func TestMachineOnlineEventMigration_FP02_WriterPauseBudget(t *testing.T) {
	e := newMoeEnv(t)
	e.mustApply(t, moeFileAUp, moeMigration(t, moeFileAUp))
	const enabled = "agent_runtime_online_event_ins:O,agent_runtime_online_event_upd:O"
	const disabled = "agent_runtime_online_event_ins:D,agent_runtime_online_event_upd:D"

	type step struct {
		name, sql, before, after string
		generationChanges        bool
	}
	steps := []step{
		{"file B", moeMigration(t, moeFileBUp), "", enabled, false},
		{"trigger-off", moeTestdata(t, moeTriggerOff), enabled, disabled, false},
		{"re-enable", moeTestdata(t, moeTriggerOn), disabled, enabled, true},
	}
	for _, s := range steps {
		// With the delay injected under the lock: statement_timeout ends the
		// file within 1.5 s of its lock, it rolls back, and nothing changed.
		g := e.generation(t)
		file, afterLock, writer := e.holdAndRun(t, moeInjectDelay(t, s.sql), e.runtime(t, "moe-fp02-"+s.name+"-d", "offline"))
		if moeSQLState(file.err) != "57014" {
			t.Fatalf("%s with a delay: err = %v, want SQLSTATE 57014 (statement_timeout)", s.name, file.err)
		}
		if afterLock > moeWriterBound {
			t.Fatalf("%s with a delay ended %v after its lock, want within 1.5 s", s.name, afterLock)
		}
		if writer.err != nil || writer.dur > moeWriterBound {
			t.Fatalf("%s with a delay: queued writer err = %v, took %v; want success within 1.5 s", s.name, writer.err, writer.dur)
		}
		if got := e.triggers(t); got != s.before {
			t.Fatalf("%s with a delay left triggers %q, want unchanged %q", s.name, got, s.before)
		}
		if got := e.generation(t); got != g {
			t.Fatalf("%s with a delay changed the generation %s to %s", s.name, g, got)
		}
		t.Logf("%s with a delay: rolled back %v after its lock; queued writer %v", s.name, afterLock, writer.dur)

		// As written: the file commits and the queued writer finishes within 1.5 s.
		file, afterLock, writer = e.holdAndRun(t, s.sql, e.runtime(t, "moe-fp02-"+s.name+"-w", "offline"))
		if file.err != nil {
			t.Fatalf("%s as written: %v", s.name, file.err)
		}
		if afterLock > moeWriterBound || writer.err != nil || writer.dur > moeWriterBound {
			t.Fatalf("%s as written: file %v after its lock, writer err = %v in %v; want both within 1.5 s", s.name, afterLock, writer.err, writer.dur)
		}
		if got := e.triggers(t); got != s.after {
			t.Fatalf("%s as written left triggers %q, want %q", s.name, got, s.after)
		}
		if changed := e.generation(t) != g; changed != s.generationChanges {
			t.Fatalf("%s as written: generation changed = %v, want %v", s.name, changed, s.generationChanges)
		}
		t.Logf("%s as written: committed %v after its lock; queued writer %v", s.name, afterLock, writer.dur)
	}
}

// FP-03: with the event inserts broken, flips fail closed, the trigger-off file
// still commits, and the re-enable file fails whole until the names are back.
func TestMachineOnlineEventMigration_FP03_RecoveryWithBrokenInsert(t *testing.T) {
	e := newMoeEnv(t)
	e.install(t)
	const enabled = "agent_runtime_online_event_ins:O,agent_runtime_online_event_upd:O"
	const disabled = "agent_runtime_online_event_ins:D,agent_runtime_online_event_upd:D"
	exec := func(sql string) {
		t.Helper()
		if _, err := e.pool.Exec(context.Background(), sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	status := func(id string) string {
		return e.one(t, `SELECT status FROM agent_runtime WHERE id = $1`, id)
	}
	g0 := e.generation(t)
	runtimeID := e.runtime(t, "moe-fp03", "offline")
	eventsBefore := e.one(t, `SELECT count(*)::text FROM machine_online_event`)

	exec(`ALTER TABLE machine_online_event RENAME TO machine_online_event_broken`)
	if r := e.flip(runtimeID, "online"); r.err == nil || status(runtimeID) != "offline" {
		t.Fatalf("flip with a broken event table: err = %v, status %q; want an error and offline", r.err, status(runtimeID))
	}
	e.mustApply(t, moeTriggerOff, moeTestdata(t, moeTriggerOff))
	if got := e.triggers(t); got != disabled {
		t.Fatalf("triggers after trigger-off = %q, want %q", got, disabled)
	}
	if r := e.flip(runtimeID, "online"); r.err != nil || status(runtimeID) != "online" {
		t.Fatalf("flip with both triggers disabled: err = %v, status %q; want a commit and online", r.err, status(runtimeID))
	}
	if got := e.one(t, `SELECT count(*)::text FROM machine_online_event_broken`); got != eventsBefore {
		t.Fatalf("events after the flip with triggers off = %s, want %s: no event", got, eventsBefore)
	}

	exec(`ALTER TABLE machine_online_event_generation RENAME TO machine_online_event_generation_broken`)
	if _, err := e.apply(moeTestdata(t, moeTriggerOn)); err == nil {
		t.Fatal("re-enable with the generation table renamed committed, want a failure")
	}
	if got := e.triggers(t); got != disabled {
		t.Fatalf("triggers after the failed re-enable = %q, want %q", got, disabled)
	}
	if got := e.one(t, `SELECT generation::text FROM machine_online_event_generation_broken`); got != g0 {
		t.Fatalf("generation after the failed re-enable = %s, want unchanged %s", got, g0)
	}

	exec(`ALTER TABLE machine_online_event_broken RENAME TO machine_online_event`)
	exec(`ALTER TABLE machine_online_event_generation_broken RENAME TO machine_online_event_generation`)
	e.mustApply(t, moeTriggerOn, moeTestdata(t, moeTriggerOn))
	if got := e.triggers(t); got != enabled {
		t.Fatalf("triggers after the restored re-enable = %q, want %q", got, enabled)
	}
	if g1 := e.generation(t); g1 == g0 {
		t.Fatalf("generation after the restored re-enable = %s, same as before", g1)
	}
}
