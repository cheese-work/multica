package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// CHE-4568 (CHE-2804 item 1): the native retry, recurring and watchdog writers
// claim through the one recovery claim store before they dispatch.

func newClaimWritersFixture(t *testing.T) ownerFixture {
	t.Helper()
	o := newOwnerFixture(t)
	o.svc.TaskSvc.RecoveryClaims = NewRecoveryClaimStore(o.Pool)
	return o
}

// failedRun seeds a failed, retryable run with its own descendants cleaned up.
func (o ownerFixture) failedRun(t *testing.T, over testutil.Cols) string {
	t.Helper()
	cols := testutil.Cols{"failure_reason": "timeout", "attempt": 1, "max_attempts": 2, "accountable_user_id": o.UserID}
	for k, v := range over {
		cols[k] = v
	}
	id := o.failedTask(t, cols)
	o.Cleanup(t, "DELETE FROM agent_task_queue WHERE parent_task_id=$1 OR retry_of_task_id=$1 OR rerun_of_task_id=$1", id)
	return id
}

func (o ownerFixture) claimCount(t *testing.T, failed, mode string) int {
	t.Helper()
	return o.Count(t, `SELECT count(*) FROM recovery_claim WHERE failed_task_id=$1 AND mode=$2`, failed, mode)
}

// derived counts every task minted from a failed run, cancelled ones included,
// so a dispatch that was later cancelled still counts as a dispatch.
func (o ownerFixture) derived(t *testing.T, failed string) int {
	t.Helper()
	return o.Count(t, `SELECT count(*) FROM agent_task_queue WHERE parent_task_id=$1 OR retry_of_task_id=$1 OR rerun_of_task_id=$1`, failed)
}

func TestNativeRetryWriterClaimsBeforeDispatch(t *testing.T) {
	ctx := context.Background()
	t.Run("FailTask", func(t *testing.T) {
		o := newClaimWritersFixture(t)
		id := o.runningTask(t, testutil.Cols{"failure_reason": nil})
		if _, err := o.svc.TaskSvc.FailTask(ctx, parseTestUUID(t, id), "boom", "", "", "", "timeout", false, "", ""); err != nil {
			t.Fatal(err)
		}
		if got := o.derived(t, id); got != 1 {
			t.Fatalf("retry children = %d, want 1", got)
		}
		if got := o.claimCount(t, id, "native_resume"); got != 1 {
			t.Fatalf("native_resume claims = %d, want 1", got)
		}
		if got := o.Count(t, `SELECT count(*) FROM recovery_claim c JOIN agent_task_queue q ON q.id=c.successor_task_id
			WHERE c.failed_task_id=$1 AND c.state='submitted' AND q.retry_of_task_id=$1`, id); got != 1 {
			t.Fatalf("claims submitted against the retry child = %d, want 1", got)
		}
	})
	t.Run("MaybeRetryFailedTask", func(t *testing.T) {
		o := newClaimWritersFixture(t)
		id := o.failedRun(t, nil)
		parent, err := o.q.GetAgentTask(ctx, parseTestUUID(t, id))
		if err != nil {
			t.Fatal(err)
		}
		child, err := o.svc.TaskSvc.MaybeRetryFailedTask(ctx, parent)
		if err != nil || child == nil {
			t.Fatalf("MaybeRetryFailedTask child=%v err=%v, want a retry", child, err)
		}
		if got := o.claimCount(t, id, "native_resume"); got != 1 {
			t.Fatalf("native_resume claims = %d, want 1", got)
		}
	})
}

func TestRecurringWriterClaimsBeforeReserving(t *testing.T) {
	o := newClaimWritersFixture(t)
	ctx := context.Background()
	wakeupID := o.wakeup(t, "every", "continuous", nil)
	failed := o.failedTask(t, wakeupContext(t, wakeupID))
	attempt := func() *providerFailureRecoveryAttempt {
		return &providerFailureRecoveryAttempt{
			FailedTaskID: parseTestUUID(t, failed), FailedAt: time.Now().UTC().Add(-2 * time.Hour), ProbeAt: time.Now().UTC(), ProbeStatus: 401,
			Scope: providerFailureRecoveryScope{Kind: "issue_wakeup", TriggerID: parseTestUUID(t, wakeupID)},
		}
	}
	reserved, err := o.svc.TaskSvc.reserveProviderFailureRecovery(ctx, o.q, attempt(), pgtype.UUID{}, dbid.NewV7())
	if err != nil || !reserved {
		t.Fatalf("first reservation reserved=%t err=%v, want true", reserved, err)
	}
	if got := o.claimCount(t, failed, "fresh_replay"); got != 1 {
		t.Fatalf("fresh_replay claims = %d, want 1", got)
	}
	// A claim already held for this failed run refuses a second dispatch even
	// when the provider_failure_recovery row is gone.
	o.Exec(t, "DELETE FROM provider_failure_recovery WHERE failed_task_id=$1", failed)
	reserved, err = o.svc.TaskSvc.reserveProviderFailureRecovery(ctx, o.q, attempt(), pgtype.UUID{}, dbid.NewV7())
	if err != nil || reserved {
		t.Fatalf("second reservation reserved=%t err=%v, want false", reserved, err)
	}
	if got := o.claimCount(t, failed, "fresh_replay"); got != 1 {
		t.Fatalf("fresh_replay claims after refusal = %d, want 1", got)
	}
}

func TestWatchdogRerunClaimsBeforeDispatch(t *testing.T) {
	o := newClaimWritersFixture(t)
	ctx := context.Background()
	failed := o.failedRun(t, nil)
	issue := parseTestUUID(t, o.issueID)
	task, err := o.svc.TaskSvc.RerunIssue(ctx, issue, parseTestUUID(t, failed), pgtype.UUID{}, pgtype.UUID{}, false, nil)
	if err != nil || task == nil {
		t.Fatalf("RerunIssue task=%v err=%v, want a rerun", task, err)
	}
	if got := o.claimCount(t, failed, "fresh_replay"); got != 1 {
		t.Fatalf("fresh_replay claims = %d, want 1", got)
	}
	if got := o.derived(t, failed); got != 1 {
		t.Fatalf("reruns = %d, want 1", got)
	}
	// A second automated rerun of the same failed run is refused, not queued.
	if _, err := o.svc.TaskSvc.RerunIssue(ctx, issue, parseTestUUID(t, failed), pgtype.UUID{}, pgtype.UUID{}, false, nil); err == nil {
		t.Fatal("second automated rerun of the same failed run was admitted")
	}
	if got := o.derived(t, failed); got != 1 {
		t.Fatalf("reruns after refusal = %d, want 1", got)
	}
}

// A human rerun is a decision, not an automatic writer: it is never claimed.
func TestHumanRerunIsNotClaimed(t *testing.T) {
	o := newClaimWritersFixture(t)
	failed := o.failedRun(t, nil)
	_, err := o.svc.TaskSvc.RerunIssue(context.Background(), parseTestUUID(t, o.issueID), parseTestUUID(t, failed), pgtype.UUID{}, parseTestUUID(t, o.UserID), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := o.Count(t, `SELECT count(*) FROM recovery_claim WHERE failed_task_id=$1`, failed); got != 0 {
		t.Fatalf("claims for a human rerun = %d, want 0", got)
	}
}

// Writers racing on one failed run produce exactly one retry.
func TestWritersRacingOnOneFailedRunProduceOneRetry(t *testing.T) {
	o := newClaimWritersFixture(t)
	ctx := context.Background()
	failed := o.failedRun(t, nil)
	issue := parseTestUUID(t, o.issueID)
	parent, err := o.q.GetAgentTask(ctx, parseTestUUID(t, failed))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, write := range []func(){
		func() { _, _ = o.svc.TaskSvc.MaybeRetryFailedTask(ctx, parent) },
		func() { _, _ = o.svc.TaskSvc.MaybeRetryFailedTask(ctx, parent) },
		func() {
			_, _ = o.svc.TaskSvc.RerunIssue(ctx, issue, parseTestUUID(t, failed), pgtype.UUID{}, pgtype.UUID{}, false, nil)
		},
		func() {
			_, _ = o.svc.TaskSvc.RerunIssue(ctx, issue, parseTestUUID(t, failed), pgtype.UUID{}, pgtype.UUID{}, false, nil)
		},
	} {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; write() }()
	}
	close(start)
	wg.Wait()
	if got := o.Count(t, `SELECT count(*) FROM recovery_claim WHERE failed_task_id=$1`, failed); got != 1 {
		t.Fatalf("claims for the failed run = %d, want 1", got)
	}
	if got := o.derived(t, failed); got != 1 {
		t.Fatalf("tasks dispatched for the failed run = %d, want 1", got)
	}
}

// One lineage, one budget: two retries after the original attempt, whichever
// writer asks for them, and nothing more.
func TestRetryBudgetIsSharedAcrossWriters(t *testing.T) {
	o := newClaimWritersFixture(t)
	ctx := context.Background()
	issue := parseTestUUID(t, o.issueID)
	root := o.failedRun(t, testutil.Cols{"max_attempts": 3})

	retry := func(failed string) *string {
		parent, err := o.q.GetAgentTask(ctx, parseTestUUID(t, failed))
		if err != nil {
			t.Fatal(err)
		}
		child, err := o.svc.TaskSvc.MaybeRetryFailedTask(ctx, parent)
		if err != nil {
			t.Fatal(err)
		}
		if child == nil {
			return nil
		}
		id := util.UUIDToString(child.ID)
		o.Cleanup(t, "DELETE FROM agent_task_queue WHERE parent_task_id=$1 OR retry_of_task_id=$1 OR rerun_of_task_id=$1", id)
		o.Exec(t, "UPDATE agent_task_queue SET status='failed', failure_reason='timeout', completed_at=now() WHERE id=$1", id)
		return &id
	}
	first := retry(root)
	if first == nil {
		t.Fatal("first native retry was refused")
	}
	second := retry(*first)
	if second == nil {
		t.Fatal("second native retry was refused")
	}
	// The lineage has now spent its two retries. Neither the watchdog rerun nor
	// the recurring reservation may add a third.
	if task, err := o.svc.TaskSvc.RerunIssue(ctx, issue, parseTestUUID(t, *second), pgtype.UUID{}, pgtype.UUID{}, false, nil); err == nil {
		t.Fatalf("watchdog rerun admitted past the shared budget: %v", task)
	}
	if got := o.Count(t, `SELECT count(*) FROM recovery_claim WHERE lineage_root_task_id=$1 AND mode='native_resume'`, root); got != 2 {
		t.Fatalf("native claims in the lineage = %d, want 2", got)
	}
	if got := o.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND (id=$2 OR parent_task_id IS NOT NULL OR rerun_of_task_id IS NOT NULL)`, o.issueID, root); got != 3 {
		t.Fatalf("tasks in the lineage = %d, want 3 (original plus two retries)", got)
	}
}
