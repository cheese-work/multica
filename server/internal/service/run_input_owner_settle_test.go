package service

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// seedOwned inserts an owned input with an explicit owner and thread.
func (e *admEnv) seedOwned(issueID, event string, owner, thread pgtype.UUID) int64 {
	e.t.Helper()
	var seq int64
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO run_input (workspace_id, issue_id, agent_id, event_id, kind, payload, state, owner_task_id, thread_key)
		VALUES ($1, $2, $3, $4, 'comment', '{}', 'owned', $5, $6) RETURNING seq`,
		e.ws, issueID, e.agent, event, owner, thread).Scan(&seq); err != nil {
		e.t.Fatalf("seed owned %s: %v", event, err)
	}
	return seq
}

// Test 4: a completed carrier hands the input it still owns to a successor of
// its own slot. The successor takes no retry budget and no parent trigger.
func TestRunInputOwnerCompletedCarrierHandsLeftoverToSuccessor(t *testing.T) {
	e := newAdmEnv(t)
	issue := e.issue("medium")
	e.enqueue(issue)
	a := e.claim()
	if a == nil {
		t.Fatal("no run claimed")
	}
	left := e.seedOwned(issue, "leftover", a.ID, zeroThread)
	e.complete(a)

	st, owner, _ := e.rowState(left)
	if st != "owned" || !owner.Valid || owner == a.ID {
		t.Fatalf("leftover: state %q owner %v, want owned by a successor of A", st, owner)
	}
	var status string
	var attempt int32
	var parent pgtype.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT status, attempt, parent_task_id FROM agent_task_queue WHERE id = $1`, owner).Scan(&status, &attempt, &parent); err != nil {
		t.Fatalf("read successor: %v", err)
	}
	// attempt is the column default of a fresh run (1), the same as A's: the
	// design text says 0, but no CreateAgentTask param sets it, and a fresh run
	// holds the full retry budget. The successor is not a retry (no parent).
	if status != "queued" || attempt != a.Attempt || parent.Valid {
		t.Fatalf("successor status %q attempt %d parent %v, want queued, attempt %d (a fresh run), no parent", status, attempt, parent, a.Attempt)
	}
}

// Test 4 (end not completed): a failed carrier parks only its own inputs.
func TestRunInputOwnerFailedCarrierParksOnlyItsOwnInputs(t *testing.T) {
	e := newAdmEnv(t)
	issue := e.issue("medium")
	x, _ := e.carrierInThread(issue)
	y, _ := e.carrierInThread(issue)
	first := e.claim()
	if first == nil {
		t.Fatal("no run claimed")
	}
	other := y.ID
	if first.ID == y.ID {
		other = x.ID
	}
	mine := e.seedOwned(issue, "mine", first.ID, zeroThread)
	e.fail(first, "agent_error")

	if st, owner, _ := e.rowState(mine); st != "parked" || owner.Valid {
		t.Fatalf("own input: state %q owner %v, want parked, no owner", st, owner)
	}
	if st, _ := e.inputOf(issue, other); st != "owned" {
		t.Fatalf("the other carrier's input is %q, want owned", st)
	}
}

// Test 5: the orphan job parks owned inputs whose owner is no active run, and
// leaves an input whose owner is active. (An owned row with a NULL owner cannot
// exist: CHECK run_input_owned_has_owner.)
func TestRunInputOwnerOrphanJobParksOnlyInactiveOwners(t *testing.T) {
	e := newAdmEnv(t)
	issue := e.issue("medium")
	active := e.enqueue(issue)
	orphan := e.seedOwned(issue, "orphan", dbid.NewV7(), zeroThread)
	kept := e.seedOwned(issue, "kept", active.ID, zeroThread)
	ended := e.enqueueAgentEnded(issue)
	gone := e.seedOwned(issue, "ended-owner", ended, zeroThread)

	if err := e.svc.ReconcileAdmission(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for name, seq := range map[string]int64{"deleted owner": orphan, "ended owner": gone} {
		if st, owner, reason := e.rowState(seq); st != "parked" || owner.Valid || reason.String != "orphaned" {
			t.Fatalf("%s: state %q owner %v reason %q, want parked, no owner, orphaned", name, st, owner, reason.String)
		}
	}
	if st, owner, _ := e.rowState(kept); st != "owned" || owner != active.ID {
		t.Fatalf("active owner: state %q owner %v, want unchanged", st, owner)
	}
}

// enqueueAgentEnded returns the id of a cancelled run of the issue's agent.
func (e *admEnv) enqueueAgentEnded(issueID string) pgtype.UUID {
	e.t.Helper()
	var id pgtype.UUID
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status)
		SELECT agent_id, runtime_id, issue_id, 'cancelled' FROM agent_task_queue WHERE issue_id = $1 LIMIT 1 RETURNING id`, issueID).Scan(&id); err != nil {
		e.t.Fatalf("seed ended run: %v", err)
	}
	return id
}

// Test 6a: a failed parent of thread T1 with three claimed rows: all re-held,
// owner = the retry child, thread T1.
func TestRunInputRetryReholdsWholeBatchToChild(t *testing.T) {
	e := newAdmEnv(t)
	issue := e.issue("medium")
	thread := e.threadComment(issue)
	parent, child := retryPair(issue, thread)
	var seqs []int64
	for _, ev := range []string{"b1", "b2", "b3"} {
		seqs = append(seqs, e.seedBatchRow(issue, ev, "claimed", parent.ID, "", thread))
	}
	if err := redeliverToRetry(context.Background(), e.svc.Queries, parent, child); err != nil {
		t.Fatalf("redeliver: %v", err)
	}
	for _, seq := range seqs {
		if st, owner, _ := e.rowState(seq); st != "owned" || owner != child.ID {
			t.Fatalf("row %d: state %q owner %v, want owned by the child", seq, st, owner)
		}
	}
}

// Test 6b: a legacy mixed-thread failed batch. Only the row of the child's
// thread moves; the T2 row and the unknown-thread row stay parked.
func TestRunInputRetryLegacyMixedBatchMovesOnlyOwnThread(t *testing.T) {
	e := newAdmEnv(t)
	issue := e.issue("medium")
	t1, t2 := e.threadComment(issue), e.threadComment(issue)
	parent, child := retryPair(issue, t1)
	p1 := e.seedBatchRow(issue, "p1", "claimed", parent.ID, "", t1)
	p2 := e.seedBatchRow(issue, "p2", "claimed", parent.ID, "", t2)
	p3 := e.seedBatchRow(issue, "p3", "claimed", parent.ID, "", pgtype.UUID{})
	if err := redeliverToRetry(context.Background(), e.svc.Queries, parent, child); err != nil {
		t.Fatalf("redeliver: %v", err)
	}
	if st, owner, _ := e.rowState(p1); st != "owned" || owner != child.ID {
		t.Fatalf("p1 (T1): state %q owner %v, want owned by the child", st, owner)
	}
	for name, seq := range map[string]int64{"p2 (T2)": p2, "p3 (unknown)": p3} {
		var task pgtype.UUID
		_ = e.pool.QueryRow(context.Background(), `SELECT task_id FROM run_input WHERE seq = $1`, seq).Scan(&task)
		if st, owner, _ := e.rowState(seq); st != "parked" || owner.Valid || task != parent.ID {
			t.Fatalf("%s: state %q owner %v task %v, want parked with task_id = parent", name, st, owner, task)
		}
	}
}

// Test 6c: no retry child (agent_error is not retried): nothing is re-held.
func TestRunInputRetryNoChildHoldsNothing(t *testing.T) {
	e := newAdmEnv(t)
	issue := e.issue("medium")
	e.enqueue(issue)
	run := e.claim()
	e.fail(run, "agent_error")
	var owned int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM run_input WHERE issue_id = $1 AND state = 'owned'`, issue).Scan(&owned); err != nil {
		t.Fatalf("count: %v", err)
	}
	if owned != 0 {
		t.Fatalf("%d rows owned after a failure with no retry child, want 0", owned)
	}
	if st := e.inputStates(issue); !eqStrings(st, []string{"parked"}) {
		t.Fatalf("inputs = %v, want [parked]", st)
	}
}

// Test 8: the retire trigger parks a `held` write and changes no existing row.
func TestRunInputRetireHeldTriggerParksHeldWriteOnly(t *testing.T) {
	e := newAdmEnv(t)
	issue := e.issue("medium")
	run := e.enqueue(issue)
	keep := e.seedOwned(issue, "keep", run.ID, zeroThread)
	var seq int64
	var state string
	var reason pgtype.Text
	var owner pgtype.UUID
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO run_input (workspace_id, issue_id, agent_id, event_id, kind, payload, state, owner_task_id)
		VALUES ($1, $2, $3, 'old-writer', 'comment', '{}', 'held', $4) RETURNING seq, state, park_reason, owner_task_id`,
		e.ws, issue, e.agent, run.ID).Scan(&seq, &state, &reason, &owner); err != nil {
		t.Fatalf("held insert: %v", err)
	}
	if state != "parked" || reason.String != "legacy_unowned" || owner.Valid {
		t.Fatalf("held write: state %q reason %q owner %v, want parked, legacy_unowned, no owner", state, reason.String, owner)
	}
	if st, o, _ := e.rowState(keep); st != "owned" || o != run.ID {
		t.Fatalf("existing owned row changed: state %q owner %v", st, o)
	}
}
