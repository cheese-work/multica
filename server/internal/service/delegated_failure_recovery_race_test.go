package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// TestDelegatedFailureRecoveryEventAndTimerRaceDispatchExactlyOnce is the D3
// regression: the event-triggered path (DispatchDelegatedFailureRecoveryComment,
// invoked from completion reconciliation) and the timer-triggered path
// (RecoverPendingDelegatedFailures, invoked from the periodic sweeper) share one
// durable recovery comment as their obligation. If both fire for the same
// obligation at nearly the same time — a real scenario, since the sweeper polls
// on an interval independent of completion events — the coordinator must still
// see exactly one recovery task, never two.
//
// The obligation must be genuinely UNCOVERED before the race starts, or both
// calls simply no-op against an already-covering task and the test proves
// nothing. seedRecoverySignal's initial dispatch leaves its task in an active
// (covering) status, so resetting that same row's status column in place —
// the earlier version of this test — still reads as covered by
// HasTaskCoveringDelegatedFailureComment and both concurrent calls skip real
// work. svc.CancelTask, by contrast, is a real terminal transition: the task
// is cancelled without ever delivering the recovery comment (the same
// undelivered-cancel case TestPlannedButUndeliveredRecoveryStaysPending
// proves leaves the obligation pending), so ListPendingDelegatedFailureRecoveries
// and HasTaskCoveringDelegatedFailureComment both agree nothing currently
// covers it — the two dispatch calls below have to genuinely contend for the
// right to create the sole replacement task.
//
// Both paths funnel into the same dispatchDelegatedFailureRecovery core, whose
// writes (MergeDelegatedFailureCommentIntoPendingTask, CreateAgentTask guarded by
// idx_one_pending_task_per_issue_agent_thread, RegisterPlannedCommentForActiveTask)
// already reuse the codebase's established dedup/claim primitives — the same ones
// exercised for ordinary comment-triggered enqueue in
// comment_duplicate_enqueue_race_test.go. This test does not add a new claim
// mechanism; it empirically proves those existing primitives also close the
// event-vs-timer window for the delegated-failure-recovery obligation
// specifically.
//
// Merely starting both goroutines from a synchronized entry barrier is not
// enough to prove they actually contended: one call can run start-to-finish
// before the other's relevant scan even executes, in which case the final row
// counts can look correct (exactly one surviving task) for the wrong reason —
// no concurrency ever happened, only sequential no-op-then-real-work. Nor is a
// rendezvous BEFORE the dedup read enough: both goroutines could pass that
// barrier together and still run their HasTaskCoveringDelegatedFailureComment
// reads sequentially, with caller A finishing its whole dispatch (task
// creation included) before caller B's read even executes — B would then
// observe covered == true from A's own write and no-op, which is sequential
// luck, not contention, even though the barrier "synchronized" the start. To
// rule that out, this test installs
// testHookAfterDelegatedFailureDedupCheckUncovered, an in-core probe both
// dispatch paths pass through immediately AFTER their shared
// HasTaskCoveringDelegatedFailureComment dedup read has executed and
// returned covered == false (the actual point of proven contention — see
// dispatchDelegatedFailureRecovery), and uses it to hold each goroutine until
// BOTH have independently observed the uncovered state. That proves the two
// calls were genuinely inside the critical section, having each read the
// same pre-write state, at the same time — not merely started around the
// same time.
func TestDelegatedFailureRecoveryEventAndTimerRaceDispatchExactlyOnce(t *testing.T) {
	f, svc := seedDelegatedFailureFixture(t)
	ctx := context.Background()
	failedID := f.insertWorkerTask(t, "failed", "comment", 1, 2)
	if _, err := f.pool.Exec(ctx, `
		UPDATE agent_task_queue
		SET failure_reason = 'agent_error.process_failure', error = 'worker exited', completed_at = now()
		WHERE id = $1`, failedID); err != nil {
		t.Fatalf("stamp failed task: %v", err)
	}
	failed, err := svc.Queries.GetAgentTask(ctx, failedID)
	if err != nil {
		t.Fatalf("load failed task: %v", err)
	}

	// recoverDelegatedTaskFailure creates the durable recovery comment and runs
	// the initial dispatch, producing one coordinator recovery task.
	if handled, err := svc.recoverDelegatedTaskFailure(ctx, failed); err != nil || !handled {
		t.Fatalf("initial recovery = handled %v err %v", handled, err)
	}
	var recoveryTaskID, recoveryCommentID pgtype.UUID
	if err := f.pool.QueryRow(ctx, `
		SELECT task.id, recovery.id
		FROM agent_task_queue task
		JOIN comment recovery ON recovery.id = task.trigger_comment_id
		WHERE task.trigger_evidence_kind = 'delegated_failure'
		  AND task.trigger_evidence_ref_id = $1`, failedID).Scan(&recoveryTaskID, &recoveryCommentID); err != nil {
		t.Fatalf("load initial recovery task/comment: %v", err)
	}

	// Cancel the sole recovery task before it ever delivers the comment. This
	// is a genuine terminal, non-covering transition (see
	// TestPendingDelegatedFailureSweepRequeuesTerminalUndeliveredTask's
	// cancelled case): the obligation is left pending, not merely relabeled,
	// so both dispatch paths below start from a state where neither has
	// anything to observe from the other for free.
	if _, err := svc.CancelTask(ctx, recoveryTaskID); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	var acknowledged bool
	if err := f.pool.QueryRow(ctx, `
		SELECT $2::uuid = ANY(delivered_comment_ids)
		FROM agent_task_queue WHERE id = $1`, recoveryTaskID, recoveryCommentID).Scan(&acknowledged); err != nil {
		t.Fatalf("read cancel acknowledgement: %v", err)
	}
	if acknowledged {
		t.Fatal("server-cancelling the undelivered recovery task must not acknowledge it; the obligation must stay pending for this race to be real")
	}

	comment, err := svc.Queries.GetComment(ctx, recoveryCommentID)
	if err != nil {
		t.Fatalf("load recovery comment: %v", err)
	}

	// dedupBarrier proves both goroutines have each independently observed
	// covered == false from HasTaskCoveringDelegatedFailureComment before
	// either is allowed to proceed to its write (task creation / merge /
	// exhaustion). Rendezvousing here instead of before the read is what
	// makes this a proof of contention rather than of synchronized starts —
	// see the comment above. A plain wg.Wait() on a 2-party barrier can leak
	// a goroutine forever if only one party ever arrives (e.g. a bug makes
	// one path bail out early): the test's own t.Fatal on timeout unwinds via
	// runtime.Goexit on the test goroutine, but the second worker goroutine
	// would stay parked in Wait() holding an open DB transaction for the rest
	// of the test binary's life. A select with a timeout channel fails that
	// goroutine closed instead.
	const dedupBarrierTimeout = 5 * time.Second
	var dedupBarrier sync.WaitGroup
	dedupBarrier.Add(2)
	barrierDone := make(chan struct{})
	go func() {
		dedupBarrier.Wait()
		close(barrierDone)
	}()
	var reachedMu sync.Mutex
	reachedCount := 0
	bothReached := make(chan struct{})
	svc.testHookAfterDelegatedFailureDedupCheckUncovered = func() {
		reachedMu.Lock()
		reachedCount++
		n := reachedCount
		reachedMu.Unlock()
		if n == 2 {
			close(bothReached)
		}
		dedupBarrier.Done()
		select {
		case <-barrierDone:
		case <-time.After(dedupBarrierTimeout):
			// Do not call t.Fatal from a non-test goroutine; just stop
			// blocking so this call proceeds (unsynchronized) rather than
			// hanging forever. The missing-rendezvous outcome still gets
			// caught below by the explicit bothReached timeout, which does
			// fail the test from the test goroutine.
		}
	}
	t.Cleanup(func() { svc.testHookAfterDelegatedFailureDedupCheckUncovered = nil })

	var wg sync.WaitGroup
	var startBarrier sync.WaitGroup
	startBarrier.Add(2)
	errs := make(chan error, 2)

	wg.Add(2)
	go func() {
		defer wg.Done()
		startBarrier.Done()
		startBarrier.Wait()
		errs <- svc.DispatchDelegatedFailureRecoveryComment(ctx, comment, pgtype.UUID{})
	}()
	go func() {
		defer wg.Done()
		startBarrier.Done()
		startBarrier.Wait()
		_, sweepErr := svc.RecoverPendingDelegatedFailures(ctx, 10)
		errs <- sweepErr
	}()

	select {
	case <-bothReached:
	case <-time.After(dedupBarrierTimeout):
		t.Fatal("both event and timer dispatch paths did not both observe the uncovered dedup state concurrently — no real contention was proven")
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent dispatch returned error: %v", err)
		}
	}

	// Both dispatch paths must have actually run their dispatch behavior
	// against the pending obligation rather than one silently observing the
	// other's prior state: each should report handling the comment, not a
	// same-covering-task no-op.
	var activeRecoveryTasks int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE trigger_evidence_kind = 'delegated_failure' AND trigger_evidence_ref_id = $1
		  AND status NOT IN ('cancelled')`, failedID,
	).Scan(&activeRecoveryTasks); err != nil {
		t.Fatalf("count active recovery tasks after race: %v", err)
	}
	if activeRecoveryTasks != 1 {
		t.Fatalf("active recovery task count after racing event vs timer dispatch = %d, want exactly 1 surviving recovery", activeRecoveryTasks)
	}

	var totalRecoveryTasks int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE trigger_evidence_kind = 'delegated_failure' AND trigger_evidence_ref_id = $1`, failedID,
	).Scan(&totalRecoveryTasks); err != nil {
		t.Fatalf("count all recovery tasks after race: %v", err)
	}
	// The original cancelled task plus exactly one new replacement: proves
	// both callers reached the dispatch core (neither treated the pending
	// obligation as already covered and skipped), while the uniqueness
	// constraint still let only one of them win the replacement slot.
	if totalRecoveryTasks != 2 {
		t.Fatalf("total recovery task count (cancelled original + replacement) after race = %d, want 2; a count of 1 means one dispatch call no-opped instead of contending, a count >2 means double-dispatch", totalRecoveryTasks)
	}

	var recoveryCommentCount int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM comment
		WHERE issue_id = $1 AND author_type = 'system' AND type = 'progress_update' AND source_task_id = $2`,
		f.issueID, failedID,
	).Scan(&recoveryCommentCount); err != nil {
		t.Fatalf("count recovery comments: %v", err)
	}
	if recoveryCommentCount != 1 {
		t.Fatalf("recovery comment count after race = %d, want 1 (a second dispatch created a duplicate obligation)", recoveryCommentCount)
	}
}
