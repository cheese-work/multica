package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
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

// seedClaim writes a claim row for a failed run, as if its writer had run
// `age` ago and then stopped (crash or rollback) in the given state.
func (o ownerFixture) seedClaim(t *testing.T, failed, state, successor string, age time.Duration) {
	t.Helper()
	var succ any
	if successor != "" {
		succ = successor
	}
	o.Exec(t, `INSERT INTO recovery_claim (workspace_id, issue_id, failed_task_id, lineage_root_task_id, mode, state, successor_task_id, claimed_at)
		VALUES ($1, $2, $3, $3, 'fresh_replay', $4, $5, now() - $6::interval)`,
		o.WorkspaceID, o.issueID, failed, state, succ, age.String())
	o.Cleanup(t, "DELETE FROM recovery_claim WHERE failed_task_id=$1", failed)
}

func (o ownerFixture) claimState(t *testing.T, failed string) string {
	t.Helper()
	var state string
	if err := o.Pool.QueryRow(context.Background(), `SELECT state FROM recovery_claim WHERE failed_task_id=$1`, failed).Scan(&state); err != nil {
		return ""
	}
	return state
}

func reapTogether(t *testing.T, o ownerFixture, n int) {
	t.Helper()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := o.svc.TaskSvc.RecoveryClaims.ReapSettled(context.Background(), parseTestUUID(t, o.WorkspaceID), time.Now()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

// A crash after the dispatch committed and before MarkSubmitted leaves a stale
// claimed row. The reaper must keep it, bound to the task that was dispatched:
// deleting it would let a second writer retry the same failed run.
func TestReaperKeepsClaimWhoseDispatchCommitted(t *testing.T) {
	o := newClaimWritersFixture(t)
	ctx := context.Background()
	failed := o.failedRun(t, nil)
	child := o.derivedChild(t, failed)
	o.seedClaim(t, failed, "claimed", "", 10*time.Minute)

	reapTogether(t, o, 3)

	if got := o.claimState(t, failed); got != "submitted" {
		t.Fatalf("claim state after reap = %q, want submitted (dispatch committed)", got)
	}
	if got := o.Count(t, `SELECT count(*) FROM recovery_claim WHERE failed_task_id=$1 AND successor_task_id=$2`, failed, child); got != 1 {
		t.Fatalf("claims bound to the dispatched task = %d, want 1", got)
	}
	parent, err := o.q.GetAgentTask(ctx, parseTestUUID(t, failed))
	if err != nil {
		t.Fatal(err)
	}
	if again, err := o.svc.TaskSvc.MaybeRetryFailedTask(ctx, parent); err != nil || again != nil {
		t.Fatalf("second retry after reap = %v err=%v, want none", again, err)
	}
}

// derivedChild mints the task a writer dispatched for the failed run.
func (o ownerFixture) derivedChild(t *testing.T, failed string) string {
	t.Helper()
	id := o.Task(t, o.agentID, testutil.Cols{
		"runtime_id": testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + o.agentID + "')"),
		"issue_id":   o.issueID, "status": "queued", "parent_task_id": failed, "retry_of_task_id": failed,
	})
	return id
}

// A claim whose writer never dispatched (rolled back, crashed first) must not
// pin the failed run forever.
func TestReaperFreesClaimWhenNothingWasDispatched(t *testing.T) {
	o := newClaimWritersFixture(t)
	failed := o.failedRun(t, nil)
	o.seedClaim(t, failed, "claimed", "", 10*time.Minute)
	reapTogether(t, o, 3)
	if got := o.claimState(t, failed); got != "" {
		t.Fatalf("claim state after reap = %q, want the claim removed", got)
	}
}

// The recurring writer reserves inside the caller's transaction. When that
// transaction rolls back, the claim must not stay submitted for a task that
// does not exist: it falls back to claimed, then the reaper frees the run.
func TestRecurringRollbackAfterReservationLeavesRunRecoverable(t *testing.T) {
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
	tx := o.begin(t)
	reserved, err := o.svc.TaskSvc.reserveProviderFailureRecovery(ctx, o.q.WithTx(tx), attempt(), pgtype.UUID{}, dbid.NewV7())
	if err != nil || !reserved {
		t.Fatalf("reservation reserved=%t err=%v, want true", reserved, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got := o.claimState(t, failed); got != "claimed" {
		t.Fatalf("claim state after rollback = %q, want claimed (not submitted)", got)
	}
	o.Exec(t, `UPDATE recovery_claim SET claimed_at = now() - interval '10 minutes' WHERE failed_task_id=$1`, failed)
	reapTogether(t, o, 2)
	reserved, err = o.svc.TaskSvc.reserveProviderFailureRecovery(ctx, o.q, attempt(), pgtype.UUID{}, dbid.NewV7())
	if err != nil || !reserved {
		t.Fatalf("reservation after reap reserved=%t err=%v, want true", reserved, err)
	}
}

// Both orders spend one budget. A fresh replay is a retry of its lineage, so
// native retries of the replay's own failures count it.
func TestRetryBudgetCountsReplayBeforeNativeRetries(t *testing.T) {
	o := newClaimWritersFixture(t)
	ctx := context.Background()
	issue := parseTestUUID(t, o.issueID)
	root := o.failedRun(t, testutil.Cols{"max_attempts": 3})

	replay, err := o.svc.TaskSvc.RerunIssue(ctx, issue, parseTestUUID(t, root), pgtype.UUID{}, pgtype.UUID{}, false, nil)
	if err != nil || replay == nil {
		t.Fatalf("watchdog replay = %v err=%v, want a rerun", replay, err)
	}
	fail := func(id string) db.AgentTaskQueue {
		o.Exec(t, "UPDATE agent_task_queue SET status='failed', failure_reason='timeout', completed_at=now(), max_attempts=3 WHERE id=$1", id)
		o.Cleanup(t, "DELETE FROM agent_task_queue WHERE parent_task_id=$1 OR retry_of_task_id=$1 OR rerun_of_task_id=$1", id)
		task, err := o.q.GetAgentTask(ctx, parseTestUUID(t, id))
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	replayParent := fail(util.UUIDToString(replay.ID))
	retry, err := o.svc.TaskSvc.MaybeRetryFailedTask(ctx, replayParent)
	if err != nil || retry == nil {
		t.Fatalf("native retry of the replay = %v err=%v, want the second retry of the lineage", retry, err)
	}
	third := fail(util.UUIDToString(retry.ID))
	if again, err := o.svc.TaskSvc.MaybeRetryFailedTask(ctx, third); err != nil || again != nil {
		t.Fatalf("third retry in the lineage = %v err=%v, want none", again, err)
	}
	if got := o.Count(t, `SELECT count(*) FROM recovery_claim WHERE lineage_root_task_id=$1 AND state<>'released'`, root); got != 2 {
		t.Fatalf("claims in the lineage = %d, want 2 (one replay, one native retry)", got)
	}
}

// A running autopilot recovery keeps its paced slot however old its claim is.
func TestReaperKeepsPacedSlotOfRunningAutopilotRecovery(t *testing.T) {
	o := newClaimWritersFixture(t)
	ctx := context.Background()
	failed := o.failedRun(t, nil)
	var apID, runID string
	if err := o.Pool.QueryRow(ctx, `INSERT INTO autopilot (workspace_id, title, assignee_id, execution_mode, created_by_type, created_by_id)
		VALUES ($1, 'claim-ap', $2, 'run_only', 'member', $3) RETURNING id`, o.WorkspaceID, o.agentID, o.UserID).Scan(&apID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { o.Pool.Exec(context.Background(), `DELETE FROM autopilot WHERE id=$1`, apID) })
	if err := o.Pool.QueryRow(ctx, `INSERT INTO autopilot_run (autopilot_id, source, status) VALUES ($1, 'manual', 'running') RETURNING id`, apID).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	task := o.Task(t, o.agentID, testutil.Cols{
		"runtime_id": testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + o.agentID + "')"),
		"issue_id":   o.issueID, "status": "running", "autopilot_run_id": runID,
	})
	o.Exec(t, `INSERT INTO provider_failure_recovery (failed_task_id, trigger_kind, trigger_id, failed_at, probe_at, probe_status, changed_condition, recovery_run_id)
		VALUES ($1, 'autopilot_schedule', $2, now(), now(), 401, 'provider_server_error_to_http_401', $3)`, failed, apID, runID)
	o.seedClaim(t, failed, "submitted", "", 10*time.Minute)

	reapTogether(t, o, 2)

	if got := o.claimState(t, failed); got != "submitted" {
		t.Fatalf("claim state with a running autopilot task = %q, want submitted", got)
	}
	if got := o.Count(t, `SELECT count(*) FROM recovery_claim WHERE failed_task_id=$1 AND successor_task_id=$2`, failed, task); got != 1 {
		t.Fatalf("claim bound to the running autopilot task = %d, want 1", got)
	}
	// Once the task ends, the next reap settles it and frees the slot.
	o.Exec(t, `UPDATE agent_task_queue SET status='completed', completed_at=now() WHERE id=$1`, task)
	reapTogether(t, o, 1)
	if got := o.claimState(t, failed); got != "completed" {
		t.Fatalf("claim state after the task completed = %q, want completed", got)
	}
}

// A writer paused between its claim and its dispatch is not a dead writer. The
// reaper must not free its claim, and a writer that did lose its claim must not
// dispatch (CHE-4568 review round 3, P1-1).
func TestPausedWriterIsFencedAgainstReaper(t *testing.T) {
	ctx := context.Background()
	// pause stands in for a writer stalled past the stale window: the claim is
	// older than the window and a concurrent reaper runs before it resumes.
	pause := func(o ownerFixture, failed string) {
		o.svc.TaskSvc.afterRecoveryClaim = func(context.Context) {
			o.Exec(t, `UPDATE recovery_claim SET claimed_at = now() - interval '10 minutes' WHERE failed_task_id=$1 AND state='claimed'`, failed)
			reapTogether(t, o, 1)
		}
	}
	t.Run("NativeRetryDoesNotDispatchAfterLosingClaim", func(t *testing.T) {
		o := newClaimWritersFixture(t)
		failed := o.failedRun(t, nil)
		pause(o, failed)
		parent, err := o.q.GetAgentTask(ctx, parseTestUUID(t, failed))
		if err != nil {
			t.Fatal(err)
		}
		child, _ := o.svc.TaskSvc.MaybeRetryFailedTask(ctx, parent)
		if child != nil || o.derived(t, failed) != 0 {
			t.Fatalf("writer dispatched after its claim was reaped: child=%v derived=%d", child, o.derived(t, failed))
		}
	})
	t.Run("WatchdogRerunDoesNotDispatchAfterLosingClaim", func(t *testing.T) {
		o := newClaimWritersFixture(t)
		failed := o.failedRun(t, nil)
		pause(o, failed)
		task, _ := o.svc.TaskSvc.RerunIssue(ctx, parseTestUUID(t, o.issueID), parseTestUUID(t, failed), pgtype.UUID{}, pgtype.UUID{}, false, nil)
		if task != nil || o.derived(t, failed) != 0 {
			t.Fatalf("rerun dispatched after its claim was reaped: task=%v derived=%d", task, o.derived(t, failed))
		}
	})
	t.Run("RecurringReservationDoesNotRecordAfterLosingClaim", func(t *testing.T) {
		o := newClaimWritersFixture(t)
		wakeupID := o.wakeup(t, "every", "continuous", nil)
		failed := o.failedTask(t, wakeupContext(t, wakeupID))
		pause(o, failed)
		reserved, err := o.svc.TaskSvc.reserveProviderFailureRecovery(ctx, o.q, &providerFailureRecoveryAttempt{
			FailedTaskID: parseTestUUID(t, failed), FailedAt: time.Now().UTC().Add(-2 * time.Hour), ProbeAt: time.Now().UTC(), ProbeStatus: 401,
			Scope: providerFailureRecoveryScope{Kind: "issue_wakeup", TriggerID: parseTestUUID(t, wakeupID)},
		}, pgtype.UUID{}, dbid.NewV7())
		if err != nil || reserved {
			t.Fatalf("reserved=%t err=%v after the claim was reaped, want false", reserved, err)
		}
	})
	// A writer that holds its claim keeps it: the reaper that runs while the
	// writer is paused skips it, and the writer still dispatches exactly once.
	t.Run("ActiveWriterKeepsItsClaim", func(t *testing.T) {
		o := newClaimWritersFixture(t)
		failed := o.failedRun(t, nil)
		o.svc.TaskSvc.afterRecoveryClaim = func(context.Context) { reapTogether(t, o, 3) }
		parent, err := o.q.GetAgentTask(ctx, parseTestUUID(t, failed))
		if err != nil {
			t.Fatal(err)
		}
		child, err := o.svc.TaskSvc.MaybeRetryFailedTask(ctx, parent)
		if err != nil || child == nil || o.derived(t, failed) != 1 {
			t.Fatalf("child=%v err=%v derived=%d, want one retry", child, err, o.derived(t, failed))
		}
	})
}

// A run-only autopilot failure has no issue. Its recovery still claims through
// the shared store, keyed by the failed run (CHE-4568 review round 3, P1-2).
func TestIssuelessAutopilotRecoveryClaims(t *testing.T) {
	f, ownerID := newPrincipalFixture(t)
	f.svc.TaskSvc.RecoveryClaims = NewRecoveryClaimStore(f.Pool)
	agentID := f.privateAgentOwnedBy(t, ownerID, "recovery-schedule")
	autopilotID, triggerID := f.autopilotWithTrigger(t, agentID, ownerID, ownerID)
	failedRun, err := f.q.CreateAutopilotRun(context.Background(), db.CreateAutopilotRunParams{
		ID: dbid.NewV7(), AutopilotID: parseTestUUID(t, autopilotID), TriggerID: parseTestUUID(t, triggerID),
		Source: "schedule", Status: "running", TriggerPayload: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.Cleanup(t, "DELETE FROM autopilot_run WHERE id=$1", util.UUIDToString(failedRun.ID))
	failedTaskID := f.Task(t, agentID, testutil.Cols{
		"runtime_id":       testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + agentID + "')"),
		"autopilot_run_id": util.UUIDToString(failedRun.ID),
		"status":           "failed", "failure_reason": "agent_error.provider_server_error",
		"completed_at": testutil.Raw("now()-interval '61 minutes'"),
	})
	f.Cleanup(t, "DELETE FROM provider_failure_recovery WHERE failed_task_id=$1", failedTaskID)
	f.Cleanup(t, "DELETE FROM recovery_claim WHERE failed_task_id=$1", failedTaskID)
	f.Exec(t, "UPDATE autopilot_run SET status='failed',completed_at=now() WHERE id=$1", util.UUIDToString(failedRun.ID))
	f.svc.TaskSvc.ProviderFailureProbe = &providerFailureProbeStub{evidence: ProviderFailureProbeEvidence{CheckedAt: time.Now().UTC(), StatusCode: 401}}

	recoveryRun := f.dispatch(t, autopilotID, triggerID)
	if recoveryRun.Status != "running" {
		t.Fatalf("recovery run status = %s, want running", recoveryRun.Status)
	}
	if got := f.Count(t, `SELECT count(*) FROM recovery_claim WHERE failed_task_id=$1 AND issue_id IS NULL AND mode='fresh_replay'`, failedTaskID); got != 1 {
		t.Fatalf("issue-less claims for the failed run = %d, want 1", got)
	}
	// The claim holds the run: a second recovery of it is refused, not admitted.
	hold := &recoveryHold{}
	admitted, err := f.svc.TaskSvc.claimRecovery(context.Background(), hold, pgtype.UUID{}, util.MustParseUUID(failedTaskID), RecoveryModeFreshReplay, 2)
	if err != nil || admitted {
		t.Fatalf("second claim admitted=%t err=%v, want refused", admitted, err)
	}
}

// A claim whose writer holds its row lock is skipped by the reaper even when
// the stale window has passed, and the reaper frees it once the lock ends.
func TestReaperSkipsClaimLockedByDispatchingWriter(t *testing.T) {
	o := newClaimWritersFixture(t)
	ctx := context.Background()
	failed := o.failedRun(t, nil)
	o.seedClaim(t, failed, "claimed", "", 10*time.Minute)
	tx, err := o.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM recovery_claim WHERE failed_task_id=$1 FOR UPDATE`, failed); err != nil {
		t.Fatal(err)
	}
	reapTogether(t, o, 2)
	if got := o.claimState(t, failed); got != "claimed" {
		t.Fatalf("claim state while its writer holds the row = %q, want claimed", got)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	reapTogether(t, o, 1)
	if got := o.claimState(t, failed); got != "" {
		t.Fatalf("claim state after the writer let go = %q, want reaped", got)
	}
}

// Issue-less claims share the lineage budget and allow one replay per lineage.
func TestIssuelessClaimsAreBudgetedByLineage(t *testing.T) {
	o := newClaimWritersFixture(t)
	ctx := context.Background()
	first, second := o.failedRun(t, nil), o.failedRun(t, nil)
	for _, id := range []string{first, second} {
		o.Cleanup(t, "DELETE FROM recovery_claim WHERE failed_task_id=$1", id)
	}
	claim := func(failed string) RecoveryClaimOutcome {
		_, outcome, err := o.svc.TaskSvc.RecoveryClaims.Claim(ctx, RecoveryClaimParams{
			WorkspaceID: parseTestUUID(t, o.WorkspaceID), FailedTaskID: parseTestUUID(t, failed), Mode: RecoveryModeFreshReplay,
			NativeCeiling: 3, PacedCap: 9, Spacing: 0, Now: time.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
		return outcome
	}
	if got := claim(first); got != RecoveryAdmitted {
		t.Fatalf("first issue-less claim = %s, want admitted", got)
	}
	if got := claim(first); got != RecoveryDuplicate {
		t.Fatalf("repeat claim for one failed run = %s, want duplicate", got)
	}
	// Unrelated failed runs without an issue are separate lineages.
	if got := claim(second); got != RecoveryAdmitted {
		t.Fatalf("issue-less claim of another lineage = %s, want admitted", got)
	}
}

// pauseBeyondStaleWindow stands in for a writer that stalls after its fence for
// longer than the stale window: a reaper runs with a clock ten minutes ahead.
func pauseBeyondStaleWindow(t *testing.T, o ownerFixture) {
	t.Helper()
	o.svc.TaskSvc.afterRecoveryFence = func(context.Context) {
		if err := o.svc.TaskSvc.RecoveryClaims.ReapSettled(context.Background(), parseTestUUID(t, o.WorkspaceID), time.Now().Add(10*time.Minute)); err != nil {
			t.Error(err)
		}
	}
}

// The watchdog rerun keeps its claim locked from the fence through the enqueue
// (CHE-4568 review round 4, P1-1): a reaper running after a long pause between
// the renewal and the enqueue cannot free it.
func TestWatchdogRerunClaimSurvivesPauseAfterFence(t *testing.T) {
	o := newClaimWritersFixture(t)
	ctx := context.Background()
	failed := o.failedRun(t, nil)
	pauseBeyondStaleWindow(t, o)
	task, err := o.svc.TaskSvc.RerunIssue(ctx, parseTestUUID(t, o.issueID), parseTestUUID(t, failed), pgtype.UUID{}, pgtype.UUID{}, false, nil)
	if err != nil || task == nil {
		t.Fatalf("RerunIssue task=%v err=%v, want a rerun", task, err)
	}
	if got := o.claimState(t, failed); got != "submitted" {
		t.Fatalf("claim state after the rerun = %q, want submitted (a reaper freed it during the pause)", got)
	}
	if got := o.Count(t, `SELECT count(*) FROM recovery_claim WHERE failed_task_id=$1 AND successor_task_id=$2`, failed, util.UUIDToString(task.ID)); got != 1 {
		t.Fatalf("claims bound to the rerun = %d, want 1", got)
	}
}

// A service built without a claim store (isolated test services) must still
// rerun: the fence must not dereference the nil store (round 4, P2).
func TestWatchdogRerunWithoutClaimStoreDoesNotPanic(t *testing.T) {
	o := newClaimWritersFixture(t)
	failed := o.failedRun(t, nil)
	o.svc.TaskSvc.RecoveryClaims = nil
	task, err := o.svc.TaskSvc.RerunIssue(context.Background(), parseTestUUID(t, o.issueID), parseTestUUID(t, failed), pgtype.UUID{}, pgtype.UUID{}, false, nil)
	if err != nil || task == nil {
		t.Fatalf("RerunIssue task=%v err=%v, want a rerun", task, err)
	}
}

// A transactional recurring writer holds the claim row lock until the caller's
// transaction ends, so a refused or failed reservation must not touch the claim
// from another connection (P1-2). The call must return, not wait on its own lock.
func TestRecurringReservationDoesNotDeadlockOnItsOwnClaim(t *testing.T) {
	run := func(t *testing.T, prepare func(o ownerFixture, failed string, cancel context.CancelFunc, attempt func() *providerFailureRecoveryAttempt), wantReserved bool) {
		o := newClaimWritersFixture(t)
		wakeupID := o.wakeup(t, "every", "continuous", nil)
		failed := o.failedTask(t, wakeupContext(t, wakeupID))
		attempt := func() *providerFailureRecoveryAttempt {
			return &providerFailureRecoveryAttempt{
				FailedTaskID: parseTestUUID(t, failed), FailedAt: time.Now().UTC().Add(-2 * time.Hour), ProbeAt: time.Now().UTC(), ProbeStatus: 401,
				Scope: providerFailureRecoveryScope{Kind: "issue_wakeup", TriggerID: parseTestUUID(t, wakeupID)},
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		prepare(o, failed, cancel, attempt)
		tx := o.begin(t)
		done := make(chan struct{})
		var reserved bool
		go func() {
			defer close(done)
			reserved, _ = o.svc.TaskSvc.reserveProviderFailureRecovery(ctx, o.q.WithTx(tx), attempt(), pgtype.UUID{}, dbid.NewV7())
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = tx.Rollback(context.Background()) // release the lock so the writer can finish
			<-done
			t.Fatal("reservation waited on a claim row its own transaction holds")
		}
		if reserved != wantReserved {
			t.Fatalf("reserved=%t, want %t", reserved, wantReserved)
		}
	}
	t.Run("ReservationConflict", func(t *testing.T) {
		run(t, func(o ownerFixture, failed string, _ context.CancelFunc, attempt func() *providerFailureRecoveryAttempt) {
			// Another writer already recorded the reservation, and its claim is gone.
			if reserved, err := o.svc.TaskSvc.reserveProviderFailureRecovery(context.Background(), o.q, attempt(), pgtype.UUID{}, dbid.NewV7()); err != nil || !reserved {
				t.Fatalf("setup reservation reserved=%t err=%v", reserved, err)
			}
			o.Exec(t, `DELETE FROM recovery_claim WHERE failed_task_id=$1`, failed)
		}, false)
	})
	t.Run("WriteError", func(t *testing.T) {
		run(t, func(o ownerFixture, _ string, cancel context.CancelFunc, _ func() *providerFailureRecoveryAttempt) {
			o.svc.TaskSvc.afterRecoveryFence = func(context.Context) { cancel() }
		}, false)
	})
}
