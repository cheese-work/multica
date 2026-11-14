package main

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// Tests 7, 7b and 14 of the CHE-5266 slice 2 design: the real 999e migration
// against a scratch schema holding the slice 1 shape of run_input.

const slice1Schema = `
CREATE TABLE agent (id uuid PRIMARY KEY, workspace_id uuid NOT NULL);
CREATE TABLE comment (id uuid PRIMARY KEY, issue_id uuid NOT NULL, parent_id uuid, content text);
CREATE TABLE agent_task_queue (
    id uuid PRIMARY KEY, issue_id uuid, agent_id uuid NOT NULL, status text NOT NULL,
    comment_thread_id uuid, coalesced_comment_ids uuid[] NOT NULL DEFAULT '{}', trigger_comment_id uuid);
CREATE FUNCTION comment_thread_root_id(comment_id uuid) RETURNS uuid
LANGUAGE sql STABLE STRICT AS $$
    WITH RECURSIVE ancestors AS (
        SELECT c.id, c.parent_id, c.issue_id, ARRAY[c.id] AS path FROM comment c WHERE c.id = comment_id
        UNION ALL
        SELECT p.id, p.parent_id, p.issue_id, a.path || p.id
        FROM ancestors a JOIN comment p ON p.id = a.parent_id AND p.issue_id = a.issue_id
        WHERE NOT p.id = ANY(a.path))
    SELECT COALESCE((SELECT id FROM ancestors ORDER BY cardinality(path) DESC LIMIT 1), comment_id)
$$;
CREATE TABLE run_input (
    workspace_id uuid NOT NULL, issue_id uuid NOT NULL, agent_id uuid NOT NULL, event_id text NOT NULL,
    seq bigint GENERATED ALWAYS AS IDENTITY, kind text NOT NULL, payload jsonb NOT NULL DEFAULT '{}'::jsonb,
    state text NOT NULL DEFAULT 'held' CHECK (state IN ('held', 'claimed', 'delivered', 'parked', 'discarded')),
    task_id uuid, redelivery_of_task_id uuid, park_reason text, failure_class text,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(), updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (issue_id, agent_id, event_id));
`

type ownerFixture struct {
	t                *testing.T
	ctx              context.Context
	schema           string
	pool             *pgxpool.Pool
	ws, issue, agent string
	c1, c2, c3       string // comment roots of threads T1, T2, T3
}

func newOwnerFixture(t *testing.T) *ownerFixture {
	t.Helper()
	base := openTestPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	t.Cleanup(cancel)
	f := &ownerFixture{t: t, ctx: ctx}
	f.schema = createScratchSchema(t, ctx, base, "run_input_owner_")
	f.pool = openTestPoolWithSearchPath(t, f.schema)
	if _, err := f.pool.Exec(ctx, slice1Schema); err != nil {
		t.Fatalf("slice 1 schema: %v", err)
	}
	f.ws, f.issue, f.agent = f.id(), f.id(), f.id()
	f.exec(`INSERT INTO agent (id, workspace_id) VALUES ($1, $2)`, f.agent, f.ws)
	for _, dst := range []*string{&f.c1, &f.c2, &f.c3} {
		*dst = f.id()
		f.exec(`INSERT INTO comment (id, issue_id, content) VALUES ($1, $2, 'root')`, *dst, f.issue)
	}
	return f
}

func (f *ownerFixture) id() string { return util.UUIDToString(dbid.NewV7()) }

func (f *ownerFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("exec %q: %v", sql, err)
	}
}

func (f *ownerFixture) carrier(status string, thread *string) string {
	f.t.Helper()
	id := f.id()
	f.exec(`INSERT INTO agent_task_queue (id, issue_id, agent_id, status, comment_thread_id) VALUES ($1, $2, $3, $4, $5)`,
		id, f.issue, f.agent, status, thread)
	return id
}

// legacy seeds a slice 1 row. payload is a JSON literal.
func (f *ownerFixture) legacy(event, state, payload string, task *string, reason *string) {
	f.t.Helper()
	f.exec(`INSERT INTO run_input (workspace_id, issue_id, agent_id, event_id, kind, payload, state, task_id, park_reason)
		VALUES ($1, $2, $3, $4, 'comment', $5::jsonb, $6, $7, $8)`, f.ws, f.issue, f.agent, event, payload, state, task, reason)
}

func (f *ownerFixture) migrate(direction string, versions ...string) error {
	f.t.Helper()
	return runMigrations(f.ctx, f.pool, runOptions{
		Direction:             direction,
		Files:                 realMigrationFiles(f.t, versions, direction),
		SchemaMigrationsTable: f.schema + ".schema_migrations",
		AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
		Hooks:                 hooksForDirection(direction),
		Conditions:            conditionsForDirection(direction),
	})
}

func (f *ownerFixture) up() {
	f.t.Helper()
	if err := f.migrate("up", "999e_run_input_owner"); err != nil {
		f.t.Fatalf("migrate up: %v", err)
	}
}

type rowView struct {
	state, reason, owner, thread, task string
}

func (f *ownerFixture) row(event string) rowView {
	f.t.Helper()
	var state string
	var reason, owner, thread, task *string
	if err := f.pool.QueryRow(f.ctx, `SELECT state, park_reason, owner_task_id::text, thread_key::text, task_id::text
		FROM run_input WHERE event_id = $1`, event).Scan(&state, &reason, &owner, &thread, &task); err != nil {
		f.t.Fatalf("row %s: %v", event, err)
	}
	str := func(p *string) string {
		if p == nil {
			return "NULL"
		}
		return *p
	}
	return rowView{state, str(reason), str(owner), str(thread), str(task)}
}

func (f *ownerFixture) want(event string, state, reason, owner, thread string) {
	f.t.Helper()
	got := f.row(event)
	if got.state != state || got.reason != reason || got.owner != owner || got.thread != thread {
		f.t.Fatalf("row %s = %+v, want state=%s reason=%s owner=%s thread=%s", event, got, state, reason, owner, thread)
	}
}

func (f *ownerFixture) stateCounts() string {
	f.t.Helper()
	rows, err := f.pool.Query(f.ctx, `SELECT state || '/' || COALESCE(park_reason, '-') || ':' || count(*) FROM run_input GROUP BY state, park_reason ORDER BY 1`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

func (f *ownerFixture) total() int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM run_input`).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *ownerFixture) checksum() string {
	f.t.Helper()
	var s string
	if err := f.pool.QueryRow(f.ctx, `SELECT COALESCE(md5(string_agg(r::text, '|' ORDER BY seq)), '') FROM run_input r`).Scan(&s); err != nil {
		f.t.Fatal(err)
	}
	return s
}

func jsonTask(id string) string    { return `{"task_id":"` + id + `"}` }
func jsonComment(id string) string { return `{"comment_id":"` + id + `"}` }

// seedLegacyHeld is the seed of test 7. T1 = c1, T2 = c2, T3 = c3.
func (f *ownerFixture) seedLegacyHeld() (x, y, z string) {
	z = f.carrier("failed", &f.c1)
	x = f.carrier("queued", &f.c1)
	y = f.carrier("queued", &f.c2)
	f.legacy("a", "held", jsonTask(x), nil, nil)
	f.legacy("b", "held", jsonTask(y), nil, nil)
	f.legacy("c", "held", jsonComment(f.c2), nil, nil)
	f.legacy("d", "held", jsonComment(f.c1), nil, nil)
	f.legacy("e", "held", `{}`, nil, nil)
	f.legacy("f", "held", `{"task_id":"`+f.id()+`","comment_id":"`+f.c2+`"}`, nil, nil)
	f.legacy("g", "held", `{"task_id":"`+z+`","comment_id":"`+f.c1+`"}`, nil, nil)
	f.legacy("h", "held", jsonComment(f.c3), nil, nil)
	return x, y, z
}

// Test 7: legacy held rows get an owner only on proof of an active carrier of
// the same issue and agent. The replacement carrier of a terminal one owns nothing.
func TestRunInputOwnerMigrationLegacyEvidence(t *testing.T) {
	f := newOwnerFixture(t)
	x, y, _ := f.seedLegacyHeld()
	seeded := f.total()
	f.up()
	f.want("a", "owned", "NULL", x, f.c1)
	f.want("b", "owned", "NULL", y, f.c2)
	f.want("c", "parked", "legacy_unowned", "NULL", f.c2)
	f.want("d", "parked", "legacy_unowned", "NULL", f.c1)
	f.want("e", "parked", "legacy_unowned", "NULL", "NULL")
	f.want("f", "parked", "legacy_unowned", "NULL", f.c2)
	f.want("g", "parked", "legacy_unowned", "NULL", f.c1) // the replacement X must not own it
	f.want("h", "parked", "legacy_unowned", "NULL", f.c3)
	var held, ownedWithoutKey int
	f.pool.QueryRow(f.ctx, `SELECT count(*) FROM run_input WHERE state = 'held'`).Scan(&held)
	f.pool.QueryRow(f.ctx, `SELECT count(*) FROM run_input WHERE state = 'owned' AND (owner_task_id IS NULL OR thread_key IS NULL)`).Scan(&ownedWithoutKey)
	if held != 0 || ownedWithoutKey != 0 || f.total() != seeded {
		t.Fatalf("held=%d ownedWithoutKey=%d total=%d (seeded %d), want 0 0 and no loss", held, ownedWithoutKey, f.total(), seeded)
	}
	// A claim binds exactly the rows its task owns: X binds a, Y binds b.
	for task, want := range map[string]string{x: "a", y: "b"} {
		rows, err := f.pool.Query(f.ctx, `UPDATE run_input SET state = 'claimed', task_id = $1 WHERE owner_task_id = $1 AND state = 'owned' RETURNING event_id`, task)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var ev string
			rows.Scan(&ev)
			got = append(got, ev)
		}
		rows.Close()
		if len(got) != 1 || got[0] != want {
			t.Fatalf("claim of %s bound %v, want [%s]", task, got, want)
		}
	}
	var parked int
	f.pool.QueryRow(f.ctx, `SELECT count(*) FROM run_input WHERE state = 'parked' AND event_id IN ('c','d','e','f','g','h')`).Scan(&parked)
	if parked != 6 {
		t.Fatalf("rows c to h still parked = %d, want 6 (no claim bound them)", parked)
	}
}

// Test 7b and 14g: the backfill is idempotent, the down migration parks every
// owned row for a member, and up, down, up loses no row and leaves none held.
func TestRunInputOwnerMigrationSafety(t *testing.T) {
	f := newOwnerFixture(t)
	f.seedLegacyHeld()
	seeded := f.total()
	f.up()
	after := f.checksum()
	file, err := os.ReadFile(filepath.Join("..", "..", "migrations", "999e_run_input_owner.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(file)
	backfill := body[strings.Index(body, "-- Statement 2"):strings.Index(body, "-- I1:")]
	f.exec(backfill)
	if got := f.checksum(); got != after {
		t.Fatalf("a second run of the backfill changed rows: %s != %s", got, after)
	}
	if err := f.migrate("down", "999e_run_input_owner"); err != nil {
		t.Fatalf("down: %v", err)
	}
	var owned, held int
	f.pool.QueryRow(f.ctx, `SELECT count(*) FROM run_input WHERE state = 'owned'`).Scan(&owned)
	f.pool.QueryRow(f.ctx, `SELECT count(*) FROM run_input WHERE state = 'held'`).Scan(&held)
	if owned != 0 || held != 0 || f.total() != seeded {
		t.Fatalf("after down: owned=%d held=%d total=%d (seeded %d)", owned, held, f.total(), seeded)
	}
	var reason string
	if err := f.pool.QueryRow(f.ctx, `SELECT park_reason FROM run_input WHERE event_id = 'a'`).Scan(&reason); err != nil || reason != "legacy_unowned" {
		t.Fatalf("row a after down: reason=%q err=%v, want parked legacy_unowned", reason, err)
	}
	if err := f.migrate("up", "999e_run_input_owner"); err != nil {
		t.Fatalf("second up: %v", err)
	}
	f.pool.QueryRow(f.ctx, `SELECT count(*) FROM run_input WHERE state = 'held'`).Scan(&held)
	if held != 0 || f.total() != seeded {
		t.Fatalf("after up, down, up: held=%d total=%d (seeded %d)", held, f.total(), seeded)
	}
	f.want("a", "parked", "legacy_unowned", "NULL", "NULL") // down keeps it parked
}

// The partial index is built in its own single-statement CONCURRENTLY file.
func TestRunInputOwnerIndexMigrationShape(t *testing.T) {
	for _, dir := range []string{"up"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "migrations", "999f_run_input_owner_idx."+dir+".sql"))
		if err != nil {
			t.Fatal(err)
		}
		var stmts int
		for _, s := range strings.Split(string(b), ";") {
			lines := strings.Split(s, "\n")
			var code []string
			for _, l := range lines {
				if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "--") {
					code = append(code, l)
				}
			}
			if len(code) > 0 {
				stmts++
			}
		}
		if stmts != 1 || !strings.Contains(string(b), "CREATE INDEX CONCURRENTLY") {
			t.Fatalf("999f %s: %d statements, want one CREATE INDEX CONCURRENTLY", dir, stmts)
		}
	}
}

// seedClaimedBatch is the seed of test 14: A running and B queued in T1, Y
// queued in T2, and a mixed batch A claimed under slice 1.
func (f *ownerFixture) seedClaimedBatch() (a, b, y string) {
	a = f.carrier("running", &f.c1)
	b = f.carrier("queued", &f.c1)
	y = f.carrier("queued", &f.c2)
	f.legacy("a1", "claimed", jsonTask(a)[:len(jsonTask(a))-1]+`,"comment_id":"`+f.c1+`"}`, &a, nil)
	f.legacy("s1", "claimed", jsonTask(b), &a, nil)
	f.legacy("b1", "claimed", jsonTask(y), &a, nil)
	f.legacy("n1", "claimed", jsonComment(f.c1), &a, nil)
	return a, b, y
}

func (f *ownerFixture) wantQuarantined(events ...string) {
	f.t.Helper()
	for _, ev := range events {
		got := f.row(ev)
		if got.state != "parked" || got.reason != "legacy_claim_unproven" || got.owner != "NULL" {
			f.t.Fatalf("row %s = %+v, want parked/legacy_claim_unproven with no owner", ev, got)
		}
	}
}

// oldCompletion is the slice 1 DeliverRunInputs of a finished run.
func (f *ownerFixture) oldCompletion(task string) {
	f.t.Helper()
	f.exec(`UPDATE run_input SET state = 'delivered' WHERE task_id = $1 AND state = 'claimed'`, task)
}

// 14a and 14d: after the migration the old completion delivers only the row
// whose claimer is proven; it makes no difference whether the claimer is
// already terminal while its settlement is pending.
func TestRunInputOwnerMigrationQuarantinesUnprovenClaims(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		name := "running claimer"
		if terminal {
			name = "claimer completed, settlement pending"
		}
		t.Run(name, func(t *testing.T) {
			f := newOwnerFixture(t)
			a, _, _ := f.seedClaimedBatch()
			seeded := f.total()
			if terminal {
				f.exec(`UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, a)
			}
			f.up()
			f.wantQuarantined("s1", "b1", "n1")
			f.oldCompletion(a)
			if got := f.row("a1").state; got != "delivered" {
				t.Fatalf("a1 = %s, want delivered", got)
			}
			f.wantQuarantined("s1", "b1", "n1")
			if f.total() != seeded {
				t.Fatalf("row count changed: %d, want %d", f.total(), seeded)
			}
		})
	}
}

// 14e: a claimed row whose task row is gone is quarantined, whatever its thread evidence.
func TestRunInputOwnerMigrationQuarantinesClaimOfDeletedTask(t *testing.T) {
	f := newOwnerFixture(t)
	gone := f.id()
	f.legacy("g1", "claimed", `{"task_id":"`+gone+`","comment_id":"`+f.c1+`"}`, &gone, nil)
	f.up()
	f.wantQuarantined("g1")
}

// 14j: rows a failed broad claim already parked keep a proven reason and
// quarantine the unproven ones, so recovery of A never re-holds them.
func TestRunInputOwnerMigrationQuarantinesParkedBatchOfFailedClaim(t *testing.T) {
	f := newOwnerFixture(t)
	a := f.carrier("failed", &f.c1)
	b := f.carrier("queued", &f.c1)
	reason := "run_failed"
	f.legacy("a1", "parked", jsonTask(a), &a, &reason)
	f.legacy("s1", "parked", jsonTask(b), &a, &reason)
	f.legacy("d1", "parked", jsonTask(f.id()), &a, &reason)
	f.legacy("e1", "parked", jsonComment(f.c1), &a, &reason)
	seeded := f.total()
	f.up()
	if got := f.row("a1"); got.state != "parked" || got.reason != "run_failed" {
		t.Fatalf("a1 = %+v, want parked with its original reason", got)
	}
	f.wantQuarantined("s1", "d1", "e1")
	if f.total() != seeded {
		t.Fatalf("row count changed: %d, want %d", f.total(), seeded)
	}
	// Failed-run recovery of A selects by task id, thread and the absence of
	// the quarantine reason: only a1 qualifies.
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM run_input WHERE task_id = $1 AND state = 'parked'
		AND park_reason IS DISTINCT FROM 'legacy_claim_unproven' AND park_reason IS DISTINCT FROM 'legacy_unowned'`, a).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows recovery of A may re-hold = %d (err %v), want 1", n, err)
	}
}

// waitForLockWait polls until some backend of this database waits on a lock.
func (f *ownerFixture) waitForLockWait() {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		f.pool.QueryRow(f.ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND datname = current_database()`).Scan(&n)
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.t.Fatal("no backend waited on a lock")
}

// 14c, the irreversible boundary: a completion that already holds its row locks
// commits first, so all four rows are delivered and the migration leaves them be.
func TestRunInputOwnerMigrationCompletionFirstIsTheBoundary(t *testing.T) {
	f := newOwnerFixture(t)
	a, _, _ := f.seedClaimedBatch()
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(f.ctx, `UPDATE run_input SET state = 'delivered' WHERE task_id = $1 AND state = 'claimed'`, a); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.migrate("up", "999e_run_input_owner") }()
	f.waitForLockWait()
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("migration: %v", err)
	}
	for _, ev := range []string{"a1", "s1", "b1", "n1"} {
		if got := f.row(ev).state; got != "delivered" {
			t.Fatalf("%s = %s, want delivered: the completion committed before the table lock", ev, got)
		}
	}
}

// 14b: a completion that starts while the migration waits for, then holds, the
// table lock runs after it and delivers only the proven row.
func TestRunInputOwnerMigrationCompletionBehindTheTableLock(t *testing.T) {
	f := newOwnerFixture(t)
	a, _, _ := f.seedClaimedBatch()
	// An unrelated writer makes the migration's table lock wait, so the
	// completion below queues behind the migration.
	blocker, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(f.ctx, `UPDATE run_input SET updated_at = now() WHERE event_id = 'a1'`); err != nil {
		t.Fatal(err)
	}
	migrated := make(chan error, 1)
	go func() { migrated <- f.migrate("up", "999e_run_input_owner") }()
	f.waitForLockWait()
	completed := make(chan error, 1)
	go func() {
		_, err := f.pool.Exec(f.ctx, `UPDATE run_input SET state = 'delivered' WHERE task_id = $1 AND state = 'claimed'`, a)
		completed <- err
	}()
	time.Sleep(300 * time.Millisecond) // both statements now wait on the table lock
	select {
	case err := <-completed:
		t.Fatalf("completion ran before the migration finished: %v", err)
	default:
	}
	if err := blocker.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-migrated; err != nil {
		t.Fatalf("migration: %v", err)
	}
	if err := <-completed; err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("completion: %v", err)
	}
	if got := f.row("a1").state; got != "delivered" {
		t.Fatalf("a1 = %s, want delivered", got)
	}
	f.wantQuarantined("s1", "b1", "n1")
}
