package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Tests 15a to 15j of the slice 2 design (rule A5): a coalesced trigger is
// owned only by the run the caller's pending check read, only while that run is
// queued and works the trigger's own thread. Anything else is parked.

// coalesce records the trigger event as the coalescing producers do.
func (e *admEnv) coalesce(issue db.Issue, comment, carrier pgtype.UUID, event string) error {
	return e.svc.RecordCoalescedTrigger(WithEventID(context.Background(), event), issue, util.MustParseUUID(e.agent), comment, "status", carrier)
}

// coalesced is the stored row of a coalesced trigger; found is false when none was stored.
type coalesced struct {
	found  bool
	state  string
	owner  pgtype.UUID
	thread pgtype.UUID
	reason pgtype.Text
}

func (e *admEnv) coalescedRow(issueID, event string) coalesced {
	e.t.Helper()
	var c coalesced
	err := e.pool.QueryRow(context.Background(), `SELECT state, owner_task_id, thread_key, park_reason FROM run_input
		WHERE issue_id = $1 AND event_id = $2`, issueID, "event:"+event).Scan(&c.state, &c.owner, &c.thread, &c.reason)
	switch {
	case err == nil:
		c.found = true
	case errors.Is(err, pgx.ErrNoRows):
	default:
		e.t.Fatalf("read coalesced %s: %v", event, err)
	}
	return c
}

func (c coalesced) wantParked(t *testing.T, what string) {
	t.Helper()
	if !c.found || c.state != "parked" || c.owner.Valid || c.thread.Valid || c.reason.String != "ambiguous_owner" {
		t.Fatalf("%s = %+v, want parked/ambiguous_owner with no owner and no thread", what, c)
	}
}

func (c coalesced) wantOwned(t *testing.T, what string, owner, thread pgtype.UUID) {
	t.Helper()
	if !c.found || c.state != "owned" || c.owner != owner || c.thread != thread {
		t.Fatalf("%s = %+v, want owned by %s in thread %s", what, c, util.UUIDToString(owner), util.UUIDToString(thread))
	}
}

// whileCarrierLocked runs an insert that must wait on the carrier's row lock,
// applies mutate in the lock-holding transaction, commits, and returns once the
// insert has finished.
func (e *admEnv) whileCarrierLocked(carrier pgtype.UUID, insert func() error, mutate func(ctx context.Context, tx pgx.Tx)) {
	e.t.Helper()
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		e.t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM agent_task_queue WHERE id = $1 FOR UPDATE`, carrier); err != nil {
		e.t.Fatalf("lock carrier: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- insert() }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		e.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND datname = current_database()`).Scan(&waiting)
		if waiting > 0 {
			break
		}
		select {
		case err := <-done:
			tx.Rollback(ctx)
			e.t.Fatalf("insert finished without waiting on the carrier lock: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			tx.Rollback(ctx)
			e.t.Fatal("insert never waited on the carrier row lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	mutate(ctx, tx)
	if err := tx.Commit(ctx); err != nil {
		e.t.Fatalf("commit: %v", err)
	}
	if err := <-done; err != nil {
		e.t.Fatalf("insert: %v", err)
	}
}

// 15a: the sole pending run works another thread, so it is not the trigger's
// carrier. An assignment-level run (no thread) is the carrier of a comment-less
// trigger.
func TestCoalesceSoleCarrierOfAnotherThreadParks(t *testing.T) {
	e := newAdmEnv(t)
	issueID := e.issue("medium")
	issue := e.issueRow(issueID)
	x, _ := e.carrierInThread(issueID)
	if err := e.coalesce(issue, pgtype.UUID{}, x.ID, "a-other"); err != nil {
		t.Fatal(err)
	}
	e.coalescedRow(issueID, "a-other").wantParked(t, "trigger beside a run of another thread")

	zeroIssueID := e.issue("medium")
	zero := e.enqueue(zeroIssueID)
	if err := e.coalesce(e.issueRow(zeroIssueID), pgtype.UUID{}, zero.ID, "a-own"); err != nil {
		t.Fatal(err)
	}
	e.coalescedRow(zeroIssueID, "a-own").wantOwned(t, "trigger beside the assignment-level run", zero.ID, zeroThread)
}

// 15b: runs of two threads wait in one slot-pair; each trigger is owned only by
// the run of its own thread.
func TestCoalesceCompetingThreadsOwnOnlyOwnThread(t *testing.T) {
	e := newAdmEnv(t)
	issueID := e.issue("medium")
	issue := e.issueRow(issueID)
	x, cx := e.carrierInThread(issueID)
	y, cy := e.carrierInThread(issueID)
	if err := e.coalesce(issue, cx, x.ID, "b-right"); err != nil {
		t.Fatal(err)
	}
	e.coalescedRow(issueID, "b-right").wantOwned(t, "trigger of T1 with the T1 run", x.ID, cx)
	if err := e.coalesce(issue, cx, y.ID, "b-wrong"); err != nil {
		t.Fatal(err)
	}
	e.coalescedRow(issueID, "b-wrong").wantParked(t, "trigger of T1 with the T2 run")
	if err := e.coalesce(issue, pgtype.UUID{}, x.ID, "b-bare"); err != nil {
		t.Fatal(err)
	}
	e.coalescedRow(issueID, "b-bare").wantParked(t, "comment-less trigger with a thread run")
	if err := e.coalesce(issue, cy, y.ID, "b-y"); err != nil {
		t.Fatal(err)
	}
	e.coalescedRow(issueID, "b-y").wantOwned(t, "trigger of T2 with the T2 run", y.ID, cy)
}

// 15c: an enqueue the unique index refused names no run, so its trigger is parked.
func TestCoalesceRefusedEnqueueParks(t *testing.T) {
	e := newAdmEnv(t)
	issueID := e.issue("medium")
	issue := e.issueRow(issueID)
	_, c := e.carrierInThread(issueID)
	_, err := e.svc.EnqueueTaskForIssue(WithEventID(context.Background(), "c-refused"), issue, c)
	if !errors.Is(err, ErrDuplicatePendingTask) {
		t.Fatalf("second enqueue in the same thread = %v, want ErrDuplicatePendingTask", err)
	}
	e.coalescedRow(issueID, "c-refused").wantParked(t, "refused enqueue")
}

// 15d: a claim that wins the carrier's row lock leaves nothing stored for the
// run it took: slice 1 hands its batch to the claim, not to this insert.
func TestCoalesceClaimBeforeInsertStoresNothing(t *testing.T) {
	e := newAdmEnv(t)
	issueID := e.issue("medium")
	issue := e.issueRow(issueID)
	x, cx := e.carrierInThread(issueID)
	e.whileCarrierLocked(x.ID, func() error { return e.coalesce(issue, cx, x.ID, "d-claimed") },
		func(ctx context.Context, tx pgx.Tx) {
			if _, err := tx.Exec(ctx, `UPDATE agent_task_queue SET status = 'dispatched', dispatched_at = now() WHERE id = $1`, x.ID); err != nil {
				t.Fatalf("claim: %v", err)
			}
		})
	if c := e.coalescedRow(issueID, "d-claimed"); c.found {
		t.Fatalf("trigger stored beside a claimed run: %+v", c)
	}
}

// 15e: a cancel that wins the row lock parks the trigger; it is not lost.
func TestCoalesceCancelBeforeInsertParks(t *testing.T) {
	e := newAdmEnv(t)
	issueID := e.issue("medium")
	issue := e.issueRow(issueID)
	x, cx := e.carrierInThread(issueID)
	e.whileCarrierLocked(x.ID, func() error { return e.coalesce(issue, cx, x.ID, "e-cancelled") },
		func(ctx context.Context, tx pgx.Tx) {
			if _, err := tx.Exec(ctx, `UPDATE agent_task_queue SET status = 'cancelled', completed_at = now() WHERE id = $1`, x.ID); err != nil {
				t.Fatalf("cancel: %v", err)
			}
		})
	e.coalescedRow(issueID, "e-cancelled").wantParked(t, "trigger of a cancelled run")
}

// 15f: no carrier id, or an id that names no run of this issue and agent, parks.
func TestCoalesceWithoutCarrierParks(t *testing.T) {
	e := newAdmEnv(t)
	issueID := e.issue("medium")
	issue := e.issueRow(issueID)
	if err := e.coalesce(issue, pgtype.UUID{}, pgtype.UUID{}, "f-none"); err != nil {
		t.Fatal(err)
	}
	e.coalescedRow(issueID, "f-none").wantParked(t, "no carrier id, no run")

	other := e.issue("medium")
	foreign := e.enqueue(other)
	e.enqueue(issueID)
	if err := e.coalesce(issue, pgtype.UUID{}, foreign.ID, "f-foreign"); err != nil {
		t.Fatal(err)
	}
	e.coalescedRow(issueID, "f-foreign").wantParked(t, "carrier of another issue")
	if err := e.coalesce(issue, pgtype.UUID{}, pgtype.UUID{}, "f-unnamed"); err != nil {
		t.Fatal(err)
	}
	e.coalescedRow(issueID, "f-unnamed").wantParked(t, "no carrier id beside a queued run")
}

// 15g: the run the check read ended and another took its slot; the trigger is
// never owned by the replacement.
func TestCoalesceReplacementCarrierNeverOwns(t *testing.T) {
	e := newAdmEnv(t)
	issueID := e.issue("medium")
	issue := e.issueRow(issueID)
	b, cb := e.carrierInThread(issueID)
	if _, err := e.svc.CancelTask(context.Background(), b.ID); err != nil {
		t.Fatalf("cancel B: %v", err)
	}
	cTask, err := e.svc.EnqueueTaskForIssue(context.Background(), issue, cb)
	if err != nil {
		t.Fatalf("replacement in the same slot: %v", err)
	}
	if err := e.coalesce(issue, cb, b.ID, "g-same"); err != nil {
		t.Fatal(err)
	}
	got := e.coalescedRow(issueID, "g-same")
	got.wantParked(t, "trigger read B, C replaced it in the same slot")
	if got.owner == cTask.ID {
		t.Fatal("replacement owns the trigger")
	}
	_, other := e.carrierInThread(issueID)
	if err := e.coalesce(issue, other, b.ID, "g-other"); err != nil {
		t.Fatal(err)
	}
	e.coalescedRow(issueID, "g-other").wantParked(t, "trigger read B, ended; another thread holds a run")
}

// 15h: each caller reads its own carrier. A run of the trigger's thread is
// preferred; a run of another thread is returned only as the fallback and then
// parks the trigger.
func TestCoalesceCallerCarrierLookup(t *testing.T) {
	e := newAdmEnv(t)
	issueID := e.issue("medium")
	issue := e.issueRow(issueID)
	x, cx := e.carrierInThread(issueID)
	y, cy := e.carrierInThread(issueID)
	agent := util.MustParseUUID(e.agent)
	lookup := func(thread pgtype.UUID) pgtype.UUID {
		t.Helper()
		id, err := e.svc.Queries.PendingCarrierForIssueAndAgent(context.Background(), db.PendingCarrierForIssueAndAgentParams{
			IssueID: issue.ID, AgentID: agent, ThreadCommentID: thread,
		})
		if err != nil {
			t.Fatalf("lookup: %v", err)
		}
		return id
	}
	if got := lookup(cy); got != y.ID {
		t.Fatalf("child-done lookup for T2 = %s, want the T2 run", util.UUIDToString(got))
	}
	if got := lookup(cx); got != x.ID {
		t.Fatalf("child-done lookup for T1 = %s, want the T1 run", util.UUIDToString(got))
	}
	// Comment-less callers (squad leader callback, status write) find no run of
	// their slot; the fallback carrier parks their trigger.
	fallback := lookup(pgtype.UUID{})
	if err := e.coalesce(issue, pgtype.UUID{}, fallback, "h-bare"); err != nil {
		t.Fatal(err)
	}
	e.coalescedRow(issueID, "h-bare").wantParked(t, "comment-less trigger, runs in other threads only")

	zeroIssueID := e.issue("medium")
	zero := e.enqueue(zeroIssueID)
	e.carrierInThread(zeroIssueID)
	zeroIssue := e.issueRow(zeroIssueID)
	svc := &IssueService{Queries: e.svc.Queries, TaskService: e.svc}
	pending, carrier := svc.hasPendingRun(context.Background(), zeroIssue.ID, agent)
	if !pending || carrier != zero.ID {
		t.Fatalf("status-path lookup = %v %s, want the assignment-level run %s", pending, util.UUIDToString(carrier), util.UUIDToString(zero.ID))
	}
	if err := e.coalesce(zeroIssue, pgtype.UUID{}, carrier, "h-status"); err != nil {
		t.Fatal(err)
	}
	e.coalescedRow(zeroIssueID, "h-status").wantOwned(t, "status trigger with the assignment-level run", zero.ID, zeroThread)
}

// 15i: a carrier of another thread that was already claimed or is running
// parks the trigger; nothing is dropped. A claimed or running carrier of the
// trigger's own thread stores nothing: slice 1 owns that batch.
func TestCoalesceWrongThreadClaimedOrRunningParks(t *testing.T) {
	for _, running := range []bool{false, true} {
		name := "dispatched"
		if running {
			name = "running"
		}
		t.Run(name, func(t *testing.T) {
			e := newAdmEnv(t)
			issueID := e.issue("medium")
			issue := e.issueRow(issueID)
			x, cx := e.carrierInThread(issueID)
			claimed := e.claim()
			if claimed == nil || claimed.ID != x.ID {
				t.Fatalf("claim = %v, want the carrier", claimed)
			}
			if running {
				e.start(claimed)
			}
			if err := e.coalesce(issue, pgtype.UUID{}, x.ID, "i-wrong"); err != nil {
				t.Fatal(err)
			}
			e.coalescedRow(issueID, "i-wrong").wantParked(t, name+" run of another thread")
			if err := e.coalesce(issue, cx, x.ID, "i-own"); err != nil {
				t.Fatal(err)
			}
			if c := e.coalescedRow(issueID, "i-own"); c.found {
				t.Fatalf("trigger stored beside its own %s run: %+v", name, c)
			}
		})
	}
}

// 15j: the carrier's thread changes while the insert waits for its lock; the
// insert reads the thread from the locked row, so the trigger is parked.
func TestCoalesceThreadChangedBeforeLockParks(t *testing.T) {
	e := newAdmEnv(t)
	issueID := e.issue("medium")
	issue := e.issueRow(issueID)
	x, cx := e.carrierInThread(issueID)
	moved := e.threadComment(issueID)
	e.whileCarrierLocked(x.ID, func() error { return e.coalesce(issue, cx, x.ID, "j-moved") },
		func(ctx context.Context, tx pgx.Tx) {
			if _, err := tx.Exec(ctx, `UPDATE agent_task_queue SET comment_thread_id = $2 WHERE id = $1`, x.ID, moved); err != nil {
				t.Fatalf("move thread: %v", err)
			}
		})
	e.coalescedRow(issueID, "j-moved").wantParked(t, "trigger whose carrier changed thread before the lock")
}
