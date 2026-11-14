package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// resumeSeed is the seed of test 9: queued X (T1) and Y (T2); parked rows
// T1 {d, g}, T2 {c, f}, T3 {h} and unknown thread {e}.
type resumeSeed struct {
	issue            string
	x, y             pgtype.UUID
	t1, t2, t3       pgtype.UUID
	d, g, c, f, h, e int64
	total            int
}

func (e *admEnv) resumeSeed() resumeSeed {
	s := resumeSeed{issue: e.issue("medium")}
	cx, t1 := e.carrierInThread(s.issue)
	cy, t2 := e.carrierInThread(s.issue)
	s.x, s.y, s.t1, s.t2, s.t3 = cx.ID, cy.ID, t1, t2, e.threadComment(s.issue)
	park := func(ev string, th pgtype.UUID) int64 {
		return e.seedBatchRow(s.issue, ev, "parked", pgtype.UUID{}, "legacy_unowned", th)
	}
	s.d, s.g = park("d", t1), park("g", t1)
	s.c, s.f = park("c", t2), park("f", t2)
	s.h = park("h", s.t3)
	s.e = park("e", pgtype.UUID{})
	var n int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM run_input WHERE issue_id = $1`, s.issue).Scan(&n)
	s.total = n
	return s
}

func (e *admEnv) wantNoLoss(s resumeSeed) {
	e.t.Helper()
	var n int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM run_input WHERE issue_id = $1`, s.issue).Scan(&n)
	if n != s.total {
		e.t.Fatalf("row count %d, want %d (no loss)", n, s.total)
	}
}

func (e *admEnv) wantOwnedBy(owner pgtype.UUID, thread pgtype.UUID, seqs ...int64) {
	e.t.Helper()
	for _, seq := range seqs {
		var th pgtype.UUID
		e.pool.QueryRow(context.Background(), `SELECT thread_key FROM run_input WHERE seq = $1`, seq).Scan(&th)
		if st, o, _ := e.rowState(seq); st != "owned" || o != owner || th != thread {
			e.t.Fatalf("row %d: state %q owner %v thread %v, want owned by %v in %v", seq, st, o, th, owner, thread)
		}
	}
}

func (e *admEnv) wantParked(seqs ...int64) {
	e.t.Helper()
	for _, seq := range seqs {
		if st, o, _ := e.rowState(seq); st != "parked" || o.Valid {
			e.t.Fatalf("row %d: state %q owner %v, want parked, no owner", seq, st, o)
		}
	}
}

func (e *admEnv) claimed(task pgtype.UUID) map[int64]bool {
	e.t.Helper()
	rows, err := e.svc.Queries.BindOwnedRunInputs(context.Background(), task)
	if err != nil {
		e.t.Fatalf("claim %v: %v", task, err)
	}
	out := map[int64]bool{}
	for _, r := range rows {
		if len(r.EventID) == 1 { // seeded rows have one-letter events; a carrier's own enqueue input is not one
			out[r.Seq.Int64] = true
		}
	}
	return out
}

func sameSet(got map[int64]bool, want ...int64) bool {
	if len(got) != len(want) {
		return false
	}
	for _, w := range want {
		if !got[w] {
			return false
		}
	}
	return true
}

// 9a: resume without a thread. Each group goes to the carrier of its own
// thread and only its own rows; the unknown group stays parked.
func TestRunInputResumeEachGroupToItsOwnCarrier(t *testing.T) {
	e := newAdmEnv(t)
	s := e.resumeSeed()
	res, err := e.svc.ResumeParkedInputs(context.Background(), util.MustParseUUID(s.issue), util.MustParseUUID(e.agent))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res.Resumed != 5 || res.SkippedUnknownThread != 1 {
		t.Fatalf("result %+v, want 5 resumed and 1 unknown-thread skip", res)
	}
	e.wantOwnedBy(s.x, s.t1, s.d, s.g)
	e.wantOwnedBy(s.y, s.t2, s.c, s.f)
	_, t3owner, _ := e.rowState(s.h)
	if th := e.threadOf(t3owner); th != s.t3 {
		t.Fatalf("T3 carrier thread %v, want %v", th, s.t3)
	}
	e.wantOwnedBy(t3owner, s.t3, s.h)
	e.wantParked(s.e)
	if !sameSet(e.claimed(s.x), s.d, s.g) || !sameSet(e.claimed(s.y), s.c, s.f) || !sameSet(e.claimed(t3owner), s.h) {
		t.Fatal("a claim bound rows of another group")
	}
	e.wantParked(s.e)
	e.wantNoLoss(s)
}

// 9b and 9c: the explicit unknown-thread recovery.
func TestRunInputResumeUnknownThreadByExactRow(t *testing.T) {
	e := newAdmEnv(t)
	s := e.resumeSeed()
	ctx := context.Background()
	iid, aid := util.MustParseUUID(s.issue), util.MustParseUUID(e.agent)

	// 9c: invalid inputs change nothing.
	other := e.issue("medium")
	foreign := e.threadComment(other)
	if _, err := e.svc.ResumeUnknownThreadInputs(ctx, iid, aid, foreign, []int64{s.e}); !errors.Is(err, ErrResumeInvalid) {
		t.Fatalf("thread of another issue: err %v, want ErrResumeInvalid", err)
	}
	for name, ids := range map[string][]int64{"a row with a thread": {s.d}, "a row that does not exist": {s.e + 100000}} {
		if _, err := e.svc.ResumeUnknownThreadInputs(ctx, iid, aid, s.t1, ids); !errors.Is(err, ErrResumeConflict) {
			t.Fatalf("%s: err %v, want ErrResumeConflict", name, err)
		}
	}
	e.wantParked(s.e, s.d, s.g, s.c, s.f, s.h)

	// 9b: e takes thread T1 and X claims it.
	res, err := e.svc.ResumeUnknownThreadInputs(ctx, iid, aid, s.t1, []int64{s.e})
	if err != nil || res.Resumed != 1 {
		t.Fatalf("resume e: %+v, %v", res, err)
	}
	e.wantOwnedBy(s.x, s.t1, s.e)
	e.wantParked(s.d, s.g, s.c, s.f, s.h)
	if !sameSet(e.claimed(s.x), s.e) {
		t.Fatal("X did not claim exactly e")
	}
	e.wantNoLoss(s)
}

// 9d and 9e: a dispatched carrier makes its group busy; discard drops what was skipped.
func TestRunInputResumeSkipsBusySlotAndDiscardDropsIt(t *testing.T) {
	e := newAdmEnv(t)
	s := e.resumeSeed()
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `UPDATE agent_task_queue SET status = 'dispatched' WHERE id = $1`, s.x); err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.ResumeParkedInputs(ctx, util.MustParseUUID(s.issue), util.MustParseUUID(e.agent))
	if err != nil {
		t.Fatal(err)
	}
	if res.SkippedSlotBusy != 2 || res.Resumed != 3 {
		t.Fatalf("result %+v, want 2 skipped as busy and 3 resumed", res)
	}
	e.wantParked(s.d, s.g, s.e)
	n, err := e.svc.DiscardParkedInputs(ctx, util.MustParseUUID(s.issue), util.MustParseUUID(e.agent))
	if err != nil || n != 3 {
		t.Fatalf("discard = %d, %v, want 3", n, err)
	}
	for _, seq := range []int64{s.d, s.g, s.e} {
		if st, _, _ := e.rowState(seq); st != "discarded" {
			t.Fatalf("row %d = %q, want discarded", seq, st)
		}
	}
	e.wantNoLoss(s)
}

// 9f: a concurrent change to a named row rolls the whole resume back, the
// carrier it inserted with it.
func TestRunInputResumeUnknownThreadConflictRollsBack(t *testing.T) {
	for _, change := range []string{"discard", "assign thread"} {
		t.Run(change, func(t *testing.T) {
			e := newAdmEnv(t)
			ctx := context.Background()
			issue := e.issue("medium")
			th := e.threadComment(issue) // no pending carrier in its slot: the resume inserts one
			row := e.seedBatchRow(issue, "u1", "parked", pgtype.UUID{}, "legacy_unowned", pgtype.UUID{})
			var carriersBefore int
			e.pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issue).Scan(&carriersBefore)

			tx, err := e.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `SELECT 1 FROM run_input WHERE seq = $1 FOR UPDATE`, row); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := e.svc.ResumeUnknownThreadInputs(ctx, util.MustParseUUID(issue), util.MustParseUUID(e.agent), th, []int64{row})
				done <- err
			}()
			deadline := time.Now().Add(10 * time.Second)
			for {
				var n int
				e.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND datname = current_database()`).Scan(&n)
				if n > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the resume never waited on the row lock")
				}
				time.Sleep(20 * time.Millisecond)
			}
			sql := `UPDATE run_input SET state = 'discarded' WHERE seq = $1`
			if change == "assign thread" {
				sql = `UPDATE run_input SET thread_key = '00000000-0000-0000-0000-000000000000' WHERE seq = $1`
			}
			if _, err := tx.Exec(ctx, sql, row); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; !errors.Is(err, ErrResumeConflict) {
				t.Fatalf("resume err = %v, want ErrResumeConflict", err)
			}
			var carriersAfter int
			e.pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issue).Scan(&carriersAfter)
			if carriersAfter != carriersBefore {
				t.Fatalf("carriers %d -> %d, want the inserted carrier rolled back", carriersBefore, carriersAfter)
			}
			if st, o, _ := e.rowState(row); o.Valid || st == "owned" {
				t.Fatalf("row after conflict: state %q owner %v, want unowned", st, o)
			}
		})
	}
}

// Test 10: resume and retry recovery of one failed run, started concurrently in
// both orders, never deadlock; every row ends owned by one carrier of its own
// thread and no row is lost.
func TestRunInputResumeAndRetryInterleaveWithoutDeadlock(t *testing.T) {
	for _, resumeFirst := range []bool{true, false} {
		name := "retry first"
		if resumeFirst {
			name = "resume first"
		}
		t.Run(name, func(t *testing.T) {
			e := newAdmEnv(t)
			ctx := context.Background()
			for i := 0; i < 20; i++ {
				issue := e.issue("medium")
				e.enqueue(issue)
				th := e.threadComment(issue)
				parent, child := retryPair(issue, th)
				parent.ID = e.insertEndedRun(issue, th, "failed", pgtype.UUID{})
				var seqs []int64
				for _, ev := range []string{"p1", "p2", "p3"} {
					seqs = append(seqs, e.seedBatchRow(issue, ev, "parked", parent.ID, "failed", th))
				}
				seqs = append(seqs, e.seedBatchRow(issue, "legacy", "parked", pgtype.UUID{}, "legacy_unowned", th))
				child.ID = dbid.NewV7()

				start := make(chan struct{})
				resume := func() error {
					<-start
					if !resumeFirst {
						time.Sleep(5 * time.Millisecond)
					}
					_, err := e.svc.ResumeParkedInputs(ctx, util.MustParseUUID(issue), util.MustParseUUID(e.agent))
					return err
				}
				retry := func() error {
					<-start
					if resumeFirst {
						time.Sleep(5 * time.Millisecond)
					}
					tx, err := e.pool.Begin(ctx)
					if err != nil {
						return err
					}
					defer tx.Rollback(ctx)
					// The retry child is created in the recovery transaction, as the real path does.
					if _, err := tx.Exec(ctx, `INSERT INTO agent_task_queue (id, agent_id, runtime_id, issue_id, status, comment_thread_id, parent_task_id)
						SELECT $2, agent_id, runtime_id, issue_id, 'queued', comment_thread_id, id FROM agent_task_queue WHERE id = $1`, parent.ID, child.ID); err != nil {
						return err
					}
					if err := redeliverToRetry(ctx, db.New(tx), parent, child); err != nil {
						return err
					}
					return tx.Commit(ctx)
				}
				errs := make(chan error, 2)
				go func() { errs <- resume() }()
				go func() { errs <- retry() }()
				close(start)
				// The partial unique index lets one carrier into the slot: a
				// loser gets 23505 and rolls back whole. Anything else, a
				// deadlock above all, fails the test.
				var lost int
				for j := 0; j < 2; j++ {
					if err := <-errs; err != nil {
						var pe *pgconn.PgError
						if !errors.As(err, &pe) || pe.Code != "23505" {
							t.Fatalf("iteration %d: %v", i, err)
						}
						lost++
					}
				}
				if lost > 1 {
					t.Fatalf("iteration %d: %d callers lost the slot, want at most 1", i, lost)
				}
				var carriers int
				e.pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status = 'queued'
					AND comment_thread_id = $2`, issue, th).Scan(&carriers)
				if carriers != 1 {
					t.Fatalf("iteration %d: %d queued carriers in the slot, want 1", i, carriers)
				}
				var total int
				e.pool.QueryRow(ctx, `SELECT count(*) FROM run_input WHERE issue_id = $1 AND event_id IN ('p1','p2','p3','legacy')`, issue).Scan(&total)
				if total != len(seqs) {
					t.Fatalf("iteration %d: %d of %d rows left", i, total, len(seqs))
				}
				for _, seq := range seqs {
					st, owner, _ := e.rowState(seq)
					switch {
					case st == "owned" && e.threadOf(owner) == th:
					case st == "parked" && !owner.Valid: // the loser's rows, or the legacy row the retry never re-holds
					default:
						t.Fatalf("iteration %d: row %d state %q owner %v, want owned by a carrier of its thread or parked", i, seq, st, owner)
					}
				}
			}
		})
	}
}

// insertEndedRun inserts a run of the issue's agent in a thread with a status and an optional parent.
func (e *admEnv) insertEndedRun(issue string, thread pgtype.UUID, status string, parent pgtype.UUID) pgtype.UUID {
	e.t.Helper()
	var id pgtype.UUID
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, comment_thread_id, parent_task_id)
		SELECT agent_id, runtime_id, issue_id, $2, $3, $4 FROM agent_task_queue WHERE issue_id = $1 LIMIT 1 RETURNING id`,
		issue, status, thread, parent).Scan(&id); err != nil {
		e.t.Fatalf("insert run: %v", err)
	}
	return id
}
