package service

import (
	"context"
	"sync"
	"testing"

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
// specifically, by starting both dispatch calls concurrently from a synchronized
// barrier so they contend inside the database rather than relying on goroutine
// scheduling luck.
func TestDelegatedFailureRecoveryEventAndTimerRaceDispatchExactlyOnce(t *testing.T) {
	f, svc := seedDelegatedFailureFixture(t)
	ctx := context.Background()

	// seedRecoverySignal fails a worker task and runs the initial
	// recoverDelegatedTaskFailure dispatch, returning the resulting coordinator
	// recovery task and its recovery comment.
	recoveryTaskID, recoveryCommentID := f.seedRecoverySignal(t, svc)

	// Cancel the sole recovery task before it ever delivers the comment. This
	// is a genuine terminal, non-covering transition (see
	// TestPlannedButUndeliveredRecoveryStaysPending): the obligation is left
	// pending, not merely relabeled, so both dispatch paths below start from a
	// state where neither has anything to observe from the other for free.
	if _, err := svc.CancelTask(ctx, recoveryTaskID); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	if f.settled(t, recoveryCommentID) {
		t.Fatal("cancelling the undelivered recovery task settled it; the obligation must stay pending for this race to be real")
	}

	comment, err := svc.Queries.GetComment(ctx, recoveryCommentID)
	if err != nil {
		t.Fatalf("load recovery comment: %v", err)
	}

	var wg sync.WaitGroup
	var barrier sync.WaitGroup
	barrier.Add(2)
	errs := make(chan error, 2)

	wg.Add(2)
	go func() {
		defer wg.Done()
		barrier.Done()
		barrier.Wait()
		errs <- svc.DispatchDelegatedFailureRecoveryComment(ctx, comment, pgtype.UUID{})
	}()
	go func() {
		defer wg.Done()
		barrier.Done()
		barrier.Wait()
		_, sweepErr := svc.RecoverPendingDelegatedFailures(ctx, 10)
		errs <- sweepErr
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent dispatch returned error: %v", err)
		}
	}

	var failedID pgtype.UUID
	if err := f.pool.QueryRow(ctx, `SELECT source_task_id FROM comment WHERE id = $1`, recoveryCommentID).Scan(&failedID); err != nil {
		t.Fatalf("load recovery comment source task id: %v", err)
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
