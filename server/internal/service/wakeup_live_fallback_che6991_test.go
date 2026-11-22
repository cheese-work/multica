package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-6991 (plan CHE-6971, step 5): fallback admission in dispatch. The item's
// status decides the firing: delivered is written only by the ack, fallback
// only by the dispatcher. A delivered item counts as joined only when the host
// run ended completed; every other end gives today's run.

func (e *liveEnv) itemStatus(t *testing.T) string {
	t.Helper()
	var s string
	e.f.QueryRow(t, `SELECT status FROM task_pr_event_delivery WHERE event_id=$1`, e.item.EventID).Scan(&s)
	return s
}

// age moves a timestamp of the item into the past.
func (e *liveEnv) age(t *testing.T, column string, seconds int) {
	t.Helper()
	e.f.Exec(t, `UPDATE task_pr_event_delivery SET `+column+`=`+column+` - make_interval(secs => $2) WHERE event_id=$1`, e.item.EventID, seconds)
}

// ack is the daemon's delivered ack.
func (e *liveEnv) ack(t *testing.T) error {
	t.Helper()
	_, err := e.f.q.AckTaskPREventDelivered(context.Background(), db.AckTaskPREventDeliveredParams{TaskID: parseTestUUID(t, e.host), EventID: e.item.EventID})
	return err
}

// cancelHost ends the host run the way the PR lifecycle does, through the real
// cancel path with its terminal settlement.
func (e *liveEnv) cancelHost(t *testing.T, reason string) {
	t.Helper()
	if _, err := e.f.svc.TaskSvc.CancelTaskWithReason(context.Background(), parseTestUUID(t, e.host), "host cancelled", reason); err != nil {
		t.Fatalf("cancel host: %v", err)
	}
}

// fellBack asserts the firing took today's path once: the item is fallback,
// one queued run took the receipts and the fire counted once, with no
// joined_live and one wakeup_join_fallback with the reason.
func (e *liveEnv) fellBack(t *testing.T, got db.IssueWakeup, reason string, ids ...pgtype.UUID) {
	t.Helper()
	if s := e.itemStatus(t); s != "fallback" {
		t.Fatalf("item status = %q, want fallback", s)
	}
	if n := wakeRuns(t, e.f, e.w.ID); n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
	if got.FireCount != 1 {
		t.Fatalf("fire_count = %d, want 1", got.FireCount)
	}
	for _, id := range ids {
		var task pgtype.UUID
		var processed bool
		e.f.QueryRow(t, `SELECT task_id, processed_at IS NOT NULL FROM issue_wakeup_receipt WHERE id=$1`, id).Scan(&task, &processed)
		if !processed || !task.Valid || util.UUIDToString(task) == e.host {
			t.Fatalf("receipt %s processed=%v task_id=%s, want consumed with the new run", util.UUIDToString(id), processed, util.UUIDToString(task))
		}
	}
	if n := e.triggered(t, "joined_live"); n != 0 {
		t.Fatalf("joined_live rows = %d, want 0", n)
	}
	if n := e.f.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='wakeup_join_fallback' AND details->>'reason'=$2 AND details->>'task_id'=$3`, e.issue, reason, e.host); n != 1 {
		t.Fatalf("wakeup_join_fallback rows with reason %s = %d, want 1", reason, n)
	}
}

func (e *liveEnv) noFallback(t *testing.T) {
	t.Helper()
	if s := e.itemStatus(t); s == "fallback" {
		t.Fatal("item went to fallback, want hold")
	}
	if n := wakeRuns(t, e.f, e.w.ID); n != 0 {
		t.Fatalf("runs = %d, want 0", n)
	}
}

// Red test from the plan: a delivered item whose host run was cancelled by a
// merged PR held the pass after step 4. Want fallback host_cancelled.
func TestDeliveredItemFallsBackWhenHostCancelled(t *testing.T) {
	e := newLiveEnv(t, WakeupInput{Mode: "continuous"})
	r := e.receipt(t)
	e.enqueue(t, r)
	e.setItem(t, "delivered")
	e.f.Exec(t, `UPDATE agent_task_queue SET status='cancelled', failure_reason='pr_merged', completed_at=clock_timestamp() WHERE id=$1`, e.host)
	got := e.pass(t)
	e.fellBack(t, got, "host_cancelled", r)
	if n := e.f.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='wakeup_join_fallback' AND details->>'failure_reason'='pr_merged'`, e.issue); n != 1 {
		t.Fatalf("the activity must carry the host run's failure_reason, rows = %d", n)
	}
	// Later passes neither run nor count again.
	again := e.pass(t)
	if n := wakeRuns(t, e.f, e.w.ID); n != 1 || again.FireCount != 1 {
		t.Fatalf("second pass: runs=%d fire_count=%d, want 1 and 1", n, again.FireCount)
	}
}

func TestDeliveredItemFallsBackWhenHostFailed(t *testing.T) {
	e := newLiveEnv(t, WakeupInput{Mode: "continuous"})
	r := e.receipt(t)
	e.enqueue(t, r)
	e.setItem(t, "delivered")
	e.f.Exec(t, `UPDATE agent_task_queue SET status='failed', failure_reason='agent_error', completed_at=clock_timestamp() WHERE id=$1`, e.host)
	e.fellBack(t, e.pass(t), "host_failed", r)
}

func TestDeliveredItemFallsBackOnEveryCancelPath(t *testing.T) {
	for _, reason := range []string{"pr_merged", "pr_closed", "review_superseded", "manual"} {
		t.Run(reason, func(t *testing.T) {
			e := newLiveEnv(t, WakeupInput{Mode: "continuous"})
			r := e.receipt(t)
			e.enqueue(t, r)
			e.setItem(t, "delivered")
			e.cancelHost(t, reason)
			e.fellBack(t, e.pass(t), "host_cancelled", r)
		})
	}
}

func TestAckFirstThenHostCompletesThenPassJoins(t *testing.T) {
	e := newLiveEnv(t, WakeupInput{Mode: "continuous"})
	r := e.receipt(t)
	e.enqueue(t, r)
	e.f.Exec(t, `UPDATE task_pr_event_delivery SET status='delivering', attempt_count=1 WHERE event_id=$1`, e.item.EventID)
	if err := e.ack(t); err != nil {
		t.Fatal(err)
	}
	e.endHost(t, "completed")
	got := e.pass(t)
	if n := wakeRuns(t, e.f, e.w.ID); n != 0 || got.FireCount != 1 || e.triggered(t, "joined_live") != 1 {
		t.Fatalf("runs=%d fire_count=%d joined_live=%d, want 0, 1, 1", n, got.FireCount, e.triggered(t, "joined_live"))
	}
	if s := e.itemStatus(t); s != "delivered" {
		t.Fatalf("item status = %q, want delivered", s)
	}
}

func TestFallbackFirstThenAckGetsNoRow(t *testing.T) {
	e := newLiveEnv(t, WakeupInput{Mode: "continuous"})
	r := e.receipt(t)
	e.enqueue(t, r)
	e.setItem(t, "failed")
	e.age(t, "updated_at", 61)
	got := e.pass(t)
	e.fellBack(t, got, "late_ack_cutoff", r)
	if err := e.ack(t); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ack after fallback = %v, want no row", err)
	}
	// Another pass after the late ack runs and counts nothing more.
	again := e.pass(t)
	if n := wakeRuns(t, e.f, e.w.ID); n != 1 || again.FireCount != 1 || e.itemStatus(t) != "fallback" {
		t.Fatalf("after the late ack: runs=%d fire_count=%d status=%s", n, again.FireCount, e.itemStatus(t))
	}
	if n := e.triggered(t, "joined_live"); n != 0 {
		t.Fatalf("joined_live rows = %d, want 0", n)
	}
}

// The ack and the admission race for the same failed item on two connections:
// exactly one of delivered or fallback wins, and at most one run starts.
func TestConcurrentAckAndAdmissionDecideOnce(t *testing.T) {
	for i := 0; i < 8; i++ {
		e := newLiveEnv(t, WakeupInput{Mode: "continuous"})
		r := e.receipt(t)
		e.enqueue(t, r)
		e.setItem(t, "failed")
		e.age(t, "updated_at", 61)
		var wg sync.WaitGroup
		start := make(chan struct{})
		var ackErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			ackErr = e.ack(t)
		}()
		go func() {
			defer wg.Done()
			<-start
			e.f.Exec(t, "UPDATE issue_wakeup SET next_fire_at=now()-interval '1 second' WHERE id=$1 AND next_fire_at IS NOT NULL", e.w.ID)
			w, err := e.f.q.LocklessWakeup(context.Background(), e.w.ID)
			if err != nil {
				t.Error(err)
				return
			}
			// A lock conflict aborts the pass; the next pass retries (below).
			_ = e.s.dispatch(context.Background(), w)
		}()
		close(start)
		wg.Wait()
		e.pass(t)
		switch s := e.itemStatus(t); s {
		case "fallback":
			if !errors.Is(ackErr, pgx.ErrNoRows) {
				t.Fatalf("round %d: fallback won but the ack = %v, want no row", i, ackErr)
			}
			if n := wakeRuns(t, e.f, e.w.ID); n != 1 {
				t.Fatalf("round %d: fallback won with %d runs, want 1", i, n)
			}
		case "delivered":
			if ackErr != nil {
				t.Fatalf("round %d: delivered but the ack = %v", i, ackErr)
			}
			if n := wakeRuns(t, e.f, e.w.ID); n != 0 {
				t.Fatalf("round %d: delivered with %d runs while the host runs, want 0", i, n)
			}
		default:
			t.Fatalf("round %d: item status = %q, want delivered or fallback", i, s)
		}
		if got := e.f.Count(t, `SELECT fire_count FROM issue_wakeup WHERE id=$1`, e.w.ID); got > 1 {
			t.Fatalf("round %d: fire_count = %d, want at most 1", i, got)
		}
	}
}

func TestProviderRejectionWhileHostRunsStartsOneRun(t *testing.T) {
	e := newLiveEnv(t, WakeupInput{Mode: "continuous"})
	r := e.receipt(t)
	e.enqueue(t, r)
	e.f.Exec(t, `UPDATE task_pr_event_delivery SET status='failed', failure_reason='injection_rejected', attempt_count=1 WHERE event_id=$1`, e.item.EventID)
	got := e.pass(t)
	e.fellBack(t, got, "rejected", r)
	for i := 0; i < 3; i++ {
		got = e.pass(t)
	}
	if n := wakeRuns(t, e.f, e.w.ID); n != 1 || got.FireCount != 1 {
		t.Fatalf("after three more passes: runs=%d fire_count=%d, want 1 and 1", n, got.FireCount)
	}
	if n := e.items(t); n != 1 {
		t.Fatalf("items = %d, want 1 (no new item)", n)
	}
}

func TestUnclaimedItemFallsBackAfter120Seconds(t *testing.T) {
	e := newLiveEnv(t, WakeupInput{Mode: "continuous"})
	r := e.receipt(t)
	e.enqueue(t, r)
	e.age(t, "created_at", 119)
	e.held(t, e.pass(t), r)
	e.age(t, "created_at", 2)
	e.fellBack(t, e.pass(t), "unclaimed", r)
}

func TestTurnEndedGraceAndCutoff(t *testing.T) {
	t.Run("never claimed falls back at once", func(t *testing.T) {
		e := newLiveEnv(t, WakeupInput{Mode: "continuous"})
		r := e.receipt(t)
		e.enqueue(t, r)
		e.f.Exec(t, `UPDATE task_pr_event_delivery SET status='failed', failure_reason='turn_ended', attempt_count=0 WHERE event_id=$1`, e.item.EventID)
		e.fellBack(t, e.pass(t), "turn_ended", r)
	})
	t.Run("claimed holds inside the 60 s grace", func(t *testing.T) {
		e := newLiveEnv(t, WakeupInput{Mode: "continuous"})
		r := e.receipt(t)
		e.enqueue(t, r)
		e.setItem(t, "failed")
		e.age(t, "updated_at", 59)
		e.held(t, e.pass(t), r)
		e.noFallback(t)
	})
	t.Run("claimed falls back after the grace", func(t *testing.T) {
		e := newLiveEnv(t, WakeupInput{Mode: "continuous"})
		r := e.receipt(t)
		e.enqueue(t, r)
		e.setItem(t, "failed")
		e.age(t, "updated_at", 61)
		e.fellBack(t, e.pass(t), "late_ack_cutoff", r)
	})
}

// Host-run end, for a merged and a closed PR: the real cancel path runs the
// terminal settlement, then the pass decides.
func TestHostRunEndByPRLifecycle(t *testing.T) {
	for _, reason := range []string{"pr_merged", "pr_closed"} {
		t.Run(reason+"/pending falls back at once", func(t *testing.T) {
			e := newLiveEnv(t, WakeupInput{Mode: "continuous"})
			r := e.receipt(t)
			e.enqueue(t, r)
			e.cancelHost(t, reason)
			if s := e.itemStatus(t); s != "failed" {
				t.Fatalf("terminal settlement left the item %q, want failed", s)
			}
			e.fellBack(t, e.pass(t), "turn_ended", r)
		})
		t.Run(reason+"/delivering without an ack falls back after the grace", func(t *testing.T) {
			e := newLiveEnv(t, WakeupInput{Mode: "continuous"})
			r := e.receipt(t)
			e.enqueue(t, r)
			e.f.Exec(t, `UPDATE task_pr_event_delivery SET status='delivering', attempt_count=1 WHERE event_id=$1`, e.item.EventID)
			e.cancelHost(t, reason)
			e.held(t, e.pass(t), r)
			e.noFallback(t)
			e.age(t, "updated_at", 61)
			e.fellBack(t, e.pass(t), "late_ack_cutoff", r)
		})
		t.Run(reason+"/late ack inside the grace then host_cancelled", func(t *testing.T) {
			e := newLiveEnv(t, WakeupInput{Mode: "continuous"})
			r := e.receipt(t)
			e.enqueue(t, r)
			e.f.Exec(t, `UPDATE task_pr_event_delivery SET status='delivering', attempt_count=1 WHERE event_id=$1`, e.item.EventID)
			e.cancelHost(t, reason)
			if err := e.ack(t); err != nil {
				t.Fatalf("late ack inside the grace: %v", err)
			}
			if s := e.itemStatus(t); s != "delivered" {
				t.Fatalf("item = %q, want delivered", s)
			}
			e.fellBack(t, e.pass(t), "host_cancelled", r)
		})
		t.Run(reason+"/delivered before the cancel falls back, counted once", func(t *testing.T) {
			e := newLiveEnv(t, WakeupInput{Mode: "continuous"})
			r := e.receipt(t)
			e.enqueue(t, r)
			e.setItem(t, "delivered")
			e.cancelHost(t, reason)
			e.fellBack(t, e.pass(t), "host_cancelled", r)
		})
		t.Run(reason+"/PR-only listing, then the join commits, then the cancel", func(t *testing.T) {
			e := newLiveEnv(t, WakeupInput{Mode: "continuous"})
			// The host run serves only the PR: its own receipts are PR wakes.
			e.f.Exec(t, `INSERT INTO issue_wakeup_receipt(id,wakeup_id,revision,event_key,event_type,payload,task_id,processed_at)
				VALUES(gen_random_uuid(),$1,$2,'github:o/r#7','pr.checks_failed','{}',$3,now())`, e.w.ID, e.w.Revision, e.host)
			listed, err := e.f.q.ListPROnlyRunIDs(context.Background(), db.ListPROnlyRunIDsParams{IssueID: e.issue, KeyPrefix: "github:o/r#7"})
			if err != nil || len(listed) != 1 || util.UUIDToString(listed[0]) != e.host {
				t.Fatalf("ListPROnlyRunIDs = %v, %v; want the host run", listed, err)
			}
			r := e.receipt(t)
			e.enqueue(t, r)
			e.setItem(t, "delivered")
			for _, id := range listed {
				if _, err := e.f.svc.TaskSvc.CancelTaskWithReason(context.Background(), id, "pull request", reason); err != nil {
					t.Fatal(err)
				}
			}
			e.fellBack(t, e.pass(t), "host_cancelled", r)
		})
		t.Run(reason+"/stopPROnlyWork still cancels a PR-only run that holds an item", func(t *testing.T) {
			e := newLiveEnv(t, WakeupInput{Mode: "continuous"})
			e.f.Exec(t, `INSERT INTO issue_wakeup_receipt(id,wakeup_id,revision,event_key,event_type,payload,task_id,processed_at)
				VALUES(gen_random_uuid(),$1,$2,'github:o/r#7','pr.checks_failed','{}',$3,now())`, e.w.ID, e.w.Revision, e.host)
			r := e.receipt(t)
			e.enqueue(t, r)
			e.setItem(t, "delivered")
			kind := "closed"
			if reason == "pr_merged" {
				kind = "merged"
			}
			// stopPROnlyWork lists through the issue's linked PR; the lifecycle
			// event names the PR by repo and number only for the key prefix.
			ev := PRLifecycleEvent{PRID: parseTestUUID(t, e.f.UserID), RepoOwner: "o", RepoName: "r", Number: 7, Kind: kind}
			if err := e.f.svc.TaskSvc.stopPROnlyWork(context.Background(), e.issue, ev); err != nil {
				t.Fatal(err)
			}
			var status string
			e.f.QueryRow(t, `SELECT status FROM agent_task_queue WHERE id=$1`, e.host).Scan(&status)
			if status != "cancelled" {
				t.Fatalf("PR-only host run status = %q, want cancelled", status)
			}
			e.fellBack(t, e.pass(t), "host_cancelled", r)
		})
		t.Run(reason+"/a PR-only run with no item is still cancelled", func(t *testing.T) {
			e := newLiveEnv(t, WakeupInput{Mode: "continuous"})
			e.f.Exec(t, `INSERT INTO issue_wakeup_receipt(id,wakeup_id,revision,event_key,event_type,payload,task_id,processed_at)
				VALUES(gen_random_uuid(),$1,$2,'github:o/r#7','pr.checks_failed','{}',$3,now())`, e.w.ID, e.w.Revision, e.host)
			kind := "closed"
			if reason == "pr_merged" {
				kind = "merged"
			}
			ev := PRLifecycleEvent{PRID: parseTestUUID(t, e.f.UserID), RepoOwner: "o", RepoName: "r", Number: 7, Kind: kind}
			if err := e.f.svc.TaskSvc.stopPROnlyWork(context.Background(), e.issue, ev); err != nil {
				t.Fatal(err)
			}
			var status string
			e.f.QueryRow(t, `SELECT status FROM agent_task_queue WHERE id=$1`, e.host).Scan(&status)
			if status != "cancelled" {
				t.Fatalf("PR-only host run status = %q, want cancelled", status)
			}
		})
		t.Run(reason+"/pending on a run whose terminal path skipped settlement", func(t *testing.T) {
			e := newLiveEnv(t, WakeupInput{Mode: "continuous"})
			r := e.receipt(t)
			e.enqueue(t, r)
			// A direct status write: no SettleTerminalTaskState ran.
			e.f.Exec(t, `UPDATE agent_task_queue SET status='cancelled', failure_reason=$2, completed_at=clock_timestamp() WHERE id=$1`, e.host, reason)
			if s := e.itemStatus(t); s != "pending" {
				t.Fatalf("item = %q, want pending", s)
			}
			e.fellBack(t, e.pass(t), "turn_ended", r)
		})
	}
}

func TestDeliveringOnASkippedSettlementHostSettlesThenFallsBack(t *testing.T) {
	e := newLiveEnv(t, WakeupInput{Mode: "continuous"})
	r := e.receipt(t)
	e.enqueue(t, r)
	e.f.Exec(t, `UPDATE task_pr_event_delivery SET status='delivering', attempt_count=1 WHERE event_id=$1`, e.item.EventID)
	e.f.Exec(t, `UPDATE agent_task_queue SET status='failed', failure_reason='agent_error', completed_at=clock_timestamp() WHERE id=$1`, e.host)
	// Settled to failed (turn_ended) in this pass, then held inside the grace.
	e.held(t, e.pass(t), r)
	if s := e.itemStatus(t); s != "failed" {
		t.Fatalf("item = %q, want failed after the pass settled it", s)
	}
	e.age(t, "updated_at", 61)
	e.fellBack(t, e.pass(t), "late_ack_cutoff", r)
}
