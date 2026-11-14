package service

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// cutoverSeed is the state after migration 999e quarantined a legacy claimed
// batch of A (design tests 14f and 14h): A running and B queued in T1, Y queued
// in T2. a is A's own valid claim; s names B (same thread as A), b names Y, n has
// no thread. s, b and n are parked / legacy_claim_unproven with task_id = A.
type cutoverSeed struct {
	issue          string
	a, b, y        pgtype.UUID
	t1, t2         pgtype.UUID
	ra, rs, rb, rn int64
	total          int
}

func (e *admEnv) cutoverSeed() cutoverSeed {
	s := cutoverSeed{issue: e.issue("medium")}
	cy, t2 := e.carrierInThread(s.issue)
	s.y, s.t2 = cy.ID, t2
	s.t1 = e.threadComment(s.issue)
	s.a = e.insertEndedRun(s.issue, s.t1, "running", pgtype.UUID{})
	s.b = e.insertEndedRun(s.issue, s.t1, "queued", pgtype.UUID{})
	if _, err := e.pool.Exec(context.Background(), `UPDATE agent_task_queue SET comment_thread_id = $2 WHERE id = ANY($1)`,
		[]pgtype.UUID{s.a, s.b}, s.t1); err != nil {
		e.t.Fatalf("set thread of A and B: %v", err)
	}
	s.ra = e.seedBatchRow(s.issue, "a", "claimed", s.a, "", s.t1)
	s.rs = e.seedBatchRow(s.issue, "s", "parked", s.a, "legacy_claim_unproven", s.t1)
	s.rb = e.seedBatchRow(s.issue, "b", "parked", s.a, "legacy_claim_unproven", s.t2)
	s.rn = e.seedBatchRow(s.issue, "n", "parked", s.a, "legacy_claim_unproven", pgtype.UUID{})
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM run_input WHERE issue_id = $1`, s.issue).Scan(&s.total)
	return s
}

func (e *admEnv) wantTotal(issue string, total int) {
	e.t.Helper()
	var n int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM run_input WHERE issue_id = $1`, issue).Scan(&n)
	if n != total {
		e.t.Fatalf("row count %d, want %d (no loss)", n, total)
	}
}

// 14f, member resume: each quarantined row goes to the run of its own thread
// by exact row; the NULL-thread row waits for the explicit recovery.
func TestRunInputCutoverResumeAfterQuarantine(t *testing.T) {
	e := newAdmEnv(t)
	s := e.cutoverSeed()
	ctx := context.Background()
	res, err := e.svc.ResumeParkedInputs(ctx, util.MustParseUUID(s.issue), util.MustParseUUID(e.agent))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res.Resumed != 2 || res.SkippedUnknownThread != 1 {
		t.Fatalf("result %+v, want 2 resumed and 1 unknown-thread skip", res)
	}
	e.wantOwnedBy(s.y, s.t2, s.rb)
	e.wantOwnedBy(s.b, s.t1, s.rs) // never A, which is running in the same thread
	e.wantParked(s.rn)
	if st, _, reason := e.rowState(s.rn); st != "parked" || reason.String != "legacy_claim_unproven" {
		t.Fatalf("n: state %q reason %q, want still quarantined", st, reason.String)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, s.a); err != nil {
		t.Fatal(err)
	}
	if !sameSet(e.claimed(s.b), s.rs) || !sameSet(e.claimed(s.y), s.rb) {
		t.Fatal("B did not claim exactly s, or Y did not claim exactly b")
	}
	e.wantParked(s.rn)
	e.wantTotal(s.issue, s.total)
}

// 14f, failed-run recovery: the retry child of A re-holds a only; the
// quarantined rows stay parked and no row is claimed across threads.
func TestRunInputCutoverFailedRunRecoveryLeavesQuarantined(t *testing.T) {
	e := newAdmEnv(t)
	s := e.cutoverSeed()
	ctx := context.Background()
	parent := db.AgentTaskQueue{ID: s.a, IssueID: util.MustParseUUID(s.issue), CommentThreadID: s.t1}
	child := db.AgentTaskQueue{ID: dbid.NewV7(), IssueID: parent.IssueID, CommentThreadID: s.t1}
	if err := redeliverToRetry(ctx, e.svc.Queries, parent, child); err != nil {
		t.Fatalf("redeliver: %v", err)
	}
	e.wantOwnedBy(child.ID, s.t1, s.ra)
	for _, seq := range []int64{s.rs, s.rb, s.rn} {
		if st, o, reason := e.rowState(seq); st != "parked" || o.Valid || reason.String != "legacy_claim_unproven" {
			t.Fatalf("row %d: state %q owner %v reason %q, want still quarantined", seq, st, o, reason.String)
		}
	}
	if !sameSet(e.claimed(child.ID), s.ra) {
		t.Fatal("the retry child did not claim exactly a")
	}
	e.wantTotal(s.issue, s.total)
}

// 14h: A completes under slice 1 SQL after the migration: s, which names B in
// the same thread, is not delivered, shows in the parked list, and B claims
// exactly s after a member resume.
func TestRunInputCutoverSameThreadRowOfBIsNotDeliveredByA(t *testing.T) {
	e := newAdmEnv(t)
	s := e.cutoverSeed()
	ctx := context.Background()
	if err := e.svc.Queries.DeliverRunInputs(ctx, s.a); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := e.rowState(s.ra); st != "delivered" {
		t.Fatalf("a = %q, want delivered", st)
	}
	if st, _, reason := e.rowState(s.rs); st != "parked" || reason.String != "legacy_claim_unproven" {
		t.Fatalf("s: state %q reason %q, want parked / legacy_claim_unproven", st, reason.String)
	}
	listed, err := e.svc.Queries.ListParkedRunInputs(ctx, db.ListParkedRunInputsParams{IssueID: util.MustParseUUID(s.issue), AgentID: util.MustParseUUID(e.agent)})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range listed {
		found = found || r.Seq.Int64 == s.rs
	}
	if !found {
		t.Fatal("ListParkedRunInputs did not return s")
	}
	if _, err := e.svc.ResumeParkedInputs(ctx, util.MustParseUUID(s.issue), util.MustParseUUID(e.agent)); err != nil {
		t.Fatal(err)
	}
	e.wantOwnedBy(s.b, s.t1, s.rs)
	if !sameSet(e.claimed(s.b), s.rs) {
		t.Fatal("B did not claim exactly s")
	}
	e.wantTotal(s.issue, s.total)
}

// 9f, two-id variant: one of two named rows is already discarded; the whole
// resume conflicts and changes no row and adds no carrier.
func TestRunInputResumeUnknownThreadTwoIDsOneDiscardedConflicts(t *testing.T) {
	e := newAdmEnv(t)
	ctx := context.Background()
	issue := e.issue("medium")
	th := e.threadComment(issue) // empty slot: a resume would insert a carrier
	u1 := e.seedBatchRow(issue, "u1", "parked", pgtype.UUID{}, "legacy_unowned", pgtype.UUID{})
	u2 := e.seedBatchRow(issue, "u2", "discarded", pgtype.UUID{}, "legacy_unowned", pgtype.UUID{})
	var carriers, total int
	count := func() {
		e.pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issue).Scan(&carriers)
		e.pool.QueryRow(ctx, `SELECT count(*) FROM run_input WHERE issue_id = $1`, issue).Scan(&total)
	}
	count()
	c0, t0 := carriers, total
	_, err := e.svc.ResumeUnknownThreadInputs(ctx, util.MustParseUUID(issue), util.MustParseUUID(e.agent), th, []int64{u1, u2})
	if !errors.Is(err, ErrResumeConflict) {
		t.Fatalf("err = %v, want ErrResumeConflict", err)
	}
	count()
	if carriers != c0 || total != t0 {
		t.Fatalf("carriers %d -> %d, rows %d -> %d: want no change", c0, carriers, t0, total)
	}
	if st, o, _ := e.rowState(u1); st != "parked" || o.Valid {
		t.Fatalf("u1: state %q owner %v, want parked", st, o)
	}
	if st, _, _ := e.rowState(u2); st != "discarded" {
		t.Fatalf("u2 = %q, want discarded", st)
	}
}
