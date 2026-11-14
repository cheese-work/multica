package service

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// Slice 1 SQL, frozen: the "old server" of the mixed-version window is real SQL
// run through its own connection, not a mock.
const (
	oldBindHeld = `UPDATE run_input SET state = 'claimed', task_id = $3, updated_at = clock_timestamp()
		WHERE issue_id = $1 AND agent_id = $2 AND state = 'held' RETURNING seq`
	oldParkHeld = `UPDATE run_input SET state = 'parked', park_reason = 'run_failed', failure_class = 'agent_error', updated_at = clock_timestamp()
		WHERE issue_id = $1 AND agent_id = $2 AND state = 'held'`
	oldPark = `UPDATE run_input SET state = 'parked', park_reason = 'run_failed', failure_class = 'agent_error', updated_at = clock_timestamp()
		WHERE task_id = $1 AND state = 'claimed'`
	oldParkOrphaned = `UPDATE run_input r SET state = 'parked', park_reason = 'orphaned', updated_at = clock_timestamp()
		WHERE r.state = 'held' AND NOT EXISTS (SELECT 1 FROM agent_task_queue t
			WHERE t.issue_id = r.issue_id AND t.agent_id = r.agent_id AND t.status IN ('queued', 'deferred'))
		RETURNING r.seq`
	oldReheld = `UPDATE run_input SET state = 'held', redelivery_of_task_id = COALESCE(redelivery_of_task_id, task_id), task_id = NULL,
		updated_at = clock_timestamp() WHERE issue_id = $1 AND agent_id = $2 AND state = 'parked' RETURNING seq`
	oldInsert = `INSERT INTO run_input (workspace_id, issue_id, agent_id, event_id, kind, payload)
		VALUES ($1, $2, $3, $4, 'comment', '{}') ON CONFLICT (issue_id, agent_id, event_id) DO NOTHING RETURNING seq`
	oldInsertCoalesced = `INSERT INTO run_input (workspace_id, issue_id, agent_id, event_id, kind, payload)
		SELECT $1, $2, $3, $4, 'comment', '{}'
		WHERE EXISTS (SELECT 1 FROM agent_task_queue t WHERE t.issue_id = $2 AND t.agent_id = $3 AND t.status = 'queued')
		ON CONFLICT (issue_id, agent_id, event_id) DO NOTHING RETURNING seq`
)

// execOld runs frozen slice 1 SQL on a connection of its own and returns the
// seqs it reports (the statements without RETURNING report none).
func (e *admEnv) execOld(sql string, args ...any) []int64 {
	e.t.Helper()
	conn, err := e.pool.Acquire(context.Background())
	if err != nil {
		e.t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	rows, err := conn.Query(context.Background(), sql, args...)
	if err != nil {
		e.t.Fatalf("old sql: %v", err)
	}
	defer rows.Close()
	var seqs []int64
	for rows.Next() {
		var s int64
		if err := rows.Scan(&s); err == nil {
			seqs = append(seqs, s)
		}
	}
	if rows.Err() != nil {
		e.t.Fatalf("old sql rows: %v", rows.Err())
	}
	return seqs
}

func (e *admEnv) noHeldRows() {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM run_input WHERE state = 'held'`).Scan(&n); err != nil || n != 0 {
		e.t.Fatalf("held rows = %d (err %v), want 0 (I2)", n, err)
	}
}

// mixedSeed is the seed of test 11: carriers X (T1) and Y (T2) of one (issue,
// agent), each with an owned input.
func (e *admEnv) mixedSeed() (issue string, x, y pgtype.UUID, ex, ey int64) {
	issue = e.issue("medium")
	cx, _ := e.carrierInThread(issue)
	cy, _ := e.carrierInThread(issue)
	x, y = cx.ID, cy.ID
	ex = e.seedOwned(issue, "ex", x, e.threadOf(x))
	ey = e.seedOwned(issue, "ey", y, e.threadOf(y))
	return
}

func (e *admEnv) threadOf(task pgtype.UUID) pgtype.UUID {
	e.t.Helper()
	var th pgtype.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT comment_thread_id FROM agent_task_queue WHERE id = $1`, task).Scan(&th); err != nil {
		e.t.Fatalf("thread of %v: %v", task, err)
	}
	return th
}

// 11a: the old claim binds nothing; the new claim of Y binds ey only.
func TestRunInputMixedVersionOldClaimBindsNothing(t *testing.T) {
	e := newAdmEnv(t)
	issue, x, y, ex, ey := e.mixedSeed()
	got := e.execOld(oldBindHeld, util.MustParseUUID(issue), util.MustParseUUID(e.agent), x)
	if len(got) != 0 {
		t.Fatalf("old claim bound %v, want nothing", got)
	}
	if st, owner, _ := e.rowState(ex); st != "owned" || owner != x {
		t.Fatalf("ex: state %q owner %v, want owned by X", st, owner)
	}
	if _, err := e.svc.Queries.BindOwnedRunInputs(context.Background(), y); err != nil {
		t.Fatalf("new claim of Y: %v", err)
	}
	var task pgtype.UUID
	e.pool.QueryRow(context.Background(), `SELECT task_id FROM run_input WHERE seq = $1`, ey).Scan(&task)
	if st, _, _ := e.rowState(ey); st != "claimed" || task != y {
		t.Fatalf("ey: state %q task %v, want claimed by Y", st, task)
	}
	if st, _, _ := e.rowState(ex); st != "owned" {
		t.Fatalf("ex = %q after Y's claim, want still owned", st)
	}
}

// 11b: the old terminal of A parks no held row and only A's claimed rows.
func TestRunInputMixedVersionOldTerminalParksOnlyClaimed(t *testing.T) {
	e := newAdmEnv(t)
	issue, x, y, ex, ey := e.mixedSeed()
	a := e.seedBatchRow(issue, "a-claimed", "claimed", x, "", e.threadOf(x))
	e.execOld(oldParkHeld, util.MustParseUUID(issue), util.MustParseUUID(e.agent))
	e.execOld(oldPark, x)
	if st, _, reason := e.rowState(a); st != "parked" || reason.String != "run_failed" {
		t.Fatalf("A's claimed row: state %q reason %q, want parked run_failed", st, reason.String)
	}
	for name, seq := range map[string]int64{"ex": ex, "ey": ey} {
		if st, _, _ := e.rowState(seq); st != "owned" {
			t.Fatalf("%s = %q after the old terminal, want owned", name, st)
		}
	}
	if _, err := e.svc.Queries.BindOwnedRunInputs(context.Background(), y); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := e.rowState(ey); st != "claimed" {
		t.Fatalf("ey = %q, want claimed by Y afterwards", st)
	}
}

// 11c: the old orphan job parks nothing; the new one parks only inactive owners.
func TestRunInputMixedVersionOldOrphanJobParksNothing(t *testing.T) {
	e := newAdmEnv(t)
	issue, _, _, ex, ey := e.mixedSeed()
	if got := e.execOld(oldParkOrphaned); len(got) != 0 {
		t.Fatalf("old orphan job parked %v, want nothing", got)
	}
	gone := e.seedOwned(issue, "gone", dbid.NewV7(), zeroThread)
	if err := e.svc.ReconcileAdmission(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st, _, reason := e.rowState(gone); st != "parked" || reason.String != "orphaned" {
		t.Fatalf("inactive owner row: state %q reason %q, want parked orphaned", st, reason.String)
	}
	for name, seq := range map[string]int64{"ex": ex, "ey": ey} {
		if st, _, _ := e.rowState(seq); st != "owned" {
			t.Fatalf("%s = %q, want owned (its carrier is queued)", name, st)
		}
	}
}

// 11d: old inserts and the old re-hold end parked and unowned; nothing is held.
func TestRunInputMixedVersionOldWritersStoreParked(t *testing.T) {
	e := newAdmEnv(t)
	issue, _, _, _, _ := e.mixedSeed()
	iid, aid, wid := util.MustParseUUID(issue), util.MustParseUUID(e.agent), e.ws
	ins := e.execOld(oldInsert, wid, iid, aid, "old-insert")
	coal := e.execOld(oldInsertCoalesced, wid, iid, aid, "old-coalesced")
	if len(ins) != 1 || len(coal) != 1 {
		t.Fatalf("old inserts returned %v and %v, want one row each", ins, coal)
	}
	for name, seq := range map[string]int64{"InsertRunInput": ins[0], "InsertCoalescedRunInput": coal[0]} {
		if st, owner, reason := e.rowState(seq); st != "parked" || owner.Valid || reason.String != "legacy_unowned" {
			t.Fatalf("%s: state %q owner %v reason %q, want parked, unowned, legacy_unowned", name, st, owner, reason.String)
		}
	}
	// A new writer that omits the state is parked too, never delivered.
	var st string
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO run_input (workspace_id, issue_id, agent_id, event_id, kind, payload)
		VALUES ($1, $2, $3, 'new-omits-state', 'comment', '{}') RETURNING state`, e.ws, issue, e.agent).Scan(&st); err != nil || st != "parked" {
		t.Fatalf("default-state insert: state %q err %v, want parked", st, err)
	}
	// The old re-hold of parked rows ends parked / legacy_unowned.
	e.execOld(oldReheld, iid, aid)
	var n int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM run_input WHERE issue_id = $1 AND event_id IN ('old-insert','old-coalesced','new-omits-state')
		AND state = 'parked' AND park_reason = 'legacy_unowned' AND owner_task_id IS NULL`, issue).Scan(&n)
	if n != 3 {
		t.Fatalf("%d of 3 old-written rows are parked/legacy_unowned after the old re-hold", n)
	}
	e.noHeldRows()
}
