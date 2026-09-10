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

	// Put the sole recovery task back to 'queued' with no delivery receipt, so
	// the obligation is pending again: RecoverPendingDelegatedFailures (timer
	// path) will pick up the comment via ListPendingDelegatedFailureRecoveries,
	// while DispatchDelegatedFailureRecoveryComment (event path) independently
	// redispatches the identical comment concurrently. Both now contend over the
	// same starting state instead of one observing the other's committed result
	// for free.
	if _, err := f.pool.Exec(ctx, `
		UPDATE agent_task_queue
		SET status = 'queued', delivered_comment_ids = '{}'::uuid[]
		WHERE id = $1`, recoveryTaskID); err != nil {
		t.Fatalf("reset recovery task to queued: %v", err)
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

	var recoveryTaskCount int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE trigger_evidence_kind = 'delegated_failure' AND trigger_evidence_ref_id = $1`, failedID,
	).Scan(&recoveryTaskCount); err != nil {
		t.Fatalf("count recovery tasks after race: %v", err)
	}
	if recoveryTaskCount != 1 {
		t.Fatalf("recovery task count after racing event vs timer dispatch = %d, want 1 (double-dispatch)", recoveryTaskCount)
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
