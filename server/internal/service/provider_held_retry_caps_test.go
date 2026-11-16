package service

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-1736 M1.3: DB-backed fixtures for the held 5xx retry's one-retry cap,
// its owner-`none` cases and its claim cooldown. M1.2 owns the retry itself.

const held5xx = "agent_error.provider_server_error"

// A task with no retry budget left gets no held retry: no child, reservation,
// receipt or notice. The cap is the task's own attempt/max_attempts.
func TestHeldRetryNoBudgetGetsNoRetry(t *testing.T) {
	for name, cols := range map[string]testutil.Cols{
		"attempt at ceiling":   {"attempt": 2, "max_attempts": 2},
		"retries disabled":     {"attempt": 1, "max_attempts": 1},
		"attempt past ceiling": {"attempt": 3, "max_attempts": 2},
	} {
		t.Run(name, func(t *testing.T) {
			o := newOwnerFixture(t)
			s := o.failWith(t, o.runningTask(t, cols), held5xx)
			s.wantFailed(t, held5xx)
			s.wantNoEffect(t)
			if s.reservations != 0 || s.notices != 0 {
				t.Fatalf("reservations=%d notices=%d, want 0, 0", s.reservations, s.notices)
			}
		})
	}
}

// One retry only: the held child, failing with a 5xx in turn, gets no second
// retry and the owner gets exactly one cap notice (plan r7: the lineage fence
// runs before the attempt budget). The sweeper entry point adds neither.
func TestHeldRetryChildFailureMintsNoSecondRetry(t *testing.T) {
	o := newOwnerFixture(t)
	ctx := context.Background()
	first := o.runningTask(t, nil)
	if s := o.failWith(t, first, held5xx); s.children != 1 {
		t.Fatalf("first failure children=%d, want one held retry", s.children)
	}
	var child string
	if err := o.Pool.QueryRow(ctx, `SELECT id::text FROM agent_task_queue WHERE retry_of_task_id=$1`, first).Scan(&child); err != nil {
		t.Fatal(err)
	}
	o.Exec(t, `UPDATE agent_task_queue SET status='running', fire_at=NULL WHERE id=$1`, child)

	s := o.failWith(t, child, held5xx)
	s.wantFailed(t, held5xx)
	if s.children != 0 || s.reservations != 0 || s.notices != 1 {
		t.Fatalf("second failure children=%d reservations=%d notices=%d, want 0, 0, 1", s.children, s.reservations, s.notices)
	}
	parent, err := o.q.GetAgentTask(ctx, parseTestUUID(t, child))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := o.svc.TaskSvc.MaybeRetryFailedTask(ctx, parent); err != nil || got != nil {
		t.Fatalf("MaybeRetryFailedTask child=%v err=%v, want none", got, err)
	}
	if n, notices := o.retryChildren(t, child), o.countNotices(t, child); n != 0 || notices != 1 {
		t.Fatalf("after sweeper replay children=%d notices=%d, want 0, 1", n, notices)
	}
}

// A 5xx on a recovery-lineage task that still has budget reaches the fence in
// FailTask: no child, no reservation, one owner notice. The sweeper entry point
// never retries a 5xx, so it adds no child.
func TestHeldRetryRecoveryLineageGetsNoRetryAndOneNotice(t *testing.T) {
	o := newOwnerFixture(t)
	ctx := context.Background()
	for _, subject := range []string{"recovery task", "retry child of recovery"} {
		t.Run(subject, func(t *testing.T) {
			recovery := o.recoveryTask(t, testutil.Cols{"max_attempts": 3})
			failed := recovery
			if subject != "recovery task" {
				failed = o.runningTask(t, testutil.Cols{"retry_of_task_id": recovery, "max_attempts": 3})
			}
			s := o.failWith(t, failed, held5xx)
			s.wantFailed(t, held5xx)
			if s.children != 0 || s.reservations != 0 || s.notices != 1 {
				t.Fatalf("children=%d reservations=%d notices=%d, want 0, 0, 1", s.children, s.reservations, s.notices)
			}
			parent, err := o.q.GetAgentTask(ctx, parseTestUUID(t, failed))
			if err != nil {
				t.Fatal(err)
			}
			if got, err := o.svc.TaskSvc.MaybeRetryFailedTask(ctx, parent); err != nil || got != nil {
				t.Fatalf("MaybeRetryFailedTask child=%v err=%v, want none", got, err)
			}
			if n, notices := o.retryChildren(t, failed), o.countNotices(t, failed); n != 0 || notices != 1 {
				t.Fatalf("after sweeper replay children=%d notices=%d, want 0, 1", n, notices)
			}
		})
	}
}

// Owner `none` for reasons other than a cap (no issue, triage, a wakeup-bound
// run): the failure commits and nothing is reserved, created or announced.
func TestHeldRetryOwnerNoneGetsNoRetryAndNoNotice(t *testing.T) {
	o := newOwnerFixture(t)
	triage := o.Issue(t, "triage", testutil.Cols{"triage_state": "pending", "assignee_type": "agent", "assignee_id": o.agentID})
	o.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id=$1", triage)
	for name, cols := range map[string]testutil.Cols{
		"no issue":               {"issue_id": nil},
		"triage issue":           {"issue_id": triage},
		"continuous wakeup":      wakeupContext(t, o.wakeup(t, "every", "continuous", nil)),
		"once wakeup (M1b owns)": wakeupContext(t, o.wakeup(t, "at", "once", nil)),
	} {
		t.Run(name, func(t *testing.T) {
			id := o.runningTask(t, cols)
			s := o.failWith(t, id, held5xx)
			s.wantFailed(t, held5xx)
			s.wantNoEffect(t)
			if s.reservations != 0 || s.notices != 0 {
				t.Fatalf("reservations=%d notices=%d, want 0, 0", s.reservations, s.notices)
			}
		})
	}
}

// CHE-1699 claim cooldown: once the breaker releases the held retry it is
// promoted, yet no claim succeeds until gatewayFailureCooldown (10 min) after
// the failed task's completed_at. The clock is faked by moving completed_at.
func TestHeldRetryClaimWaitsForGatewayCooldownAfterRelease(t *testing.T) {
	o := newOwnerFixture(t)
	ctx := context.Background()
	failedID := o.runningTask(t, nil)
	if s := o.failWith(t, failedID, held5xx); s.children != 1 {
		t.Fatalf("children=%d, want one held retry", s.children)
	}
	var childID, runtimeID, provider string
	if err := o.Pool.QueryRow(ctx, `
		SELECT id::text, runtime_id::text, context->'provider_hold'->>'provider'
		FROM agent_task_queue WHERE retry_of_task_id=$1`, failedID).Scan(&childID, &runtimeID, &provider); err != nil {
		t.Fatal(err)
	}
	o.Exec(t, `UPDATE agent_runtime SET status='online', last_seen_at=now() WHERE id=$1`, runtimeID)
	agent := parseTestUUID(t, o.agentID)
	claim := func() *db.AgentTaskQueue {
		t.Helper()
		got, err := o.svc.TaskSvc.ClaimTask(ctx, agent)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	promote := func() {
		t.Helper()
		if _, err := o.q.PromoteDueDeferredTasksForRuntime(ctx, db.PromoteDueDeferredTasksForRuntimeParams{
			RuntimeID: parseTestUUID(t, runtimeID), RuntimeStaleSecs: 3600,
		}); err != nil {
			t.Fatal(err)
		}
	}
	setCompletedAgo := func(interval string) {
		t.Helper()
		o.Exec(t, `UPDATE agent_task_queue SET completed_at = now() - $2::interval WHERE id=$1`, failedID, interval)
	}

	// Held: the breaker never released it, so it is neither promoted nor claimed
	// even a long time after the failure.
	setCompletedAgo("1 hour")
	o.Exec(t, `UPDATE agent_task_queue SET fire_at = now() - interval '1 hour' WHERE id=$1`, childID)
	promote()
	if got := claim(); got != nil {
		t.Fatalf("claimed %s while still held", util.UUIDToString(got.ID))
	}

	// Release at 1 minute after the failure: promoted, but cooling down.
	o.Exec(t, `INSERT INTO provider_breaker (workspace_id, provider, state, opened_at, next_probe_at, cool_off_s)
		VALUES ($1, $2, 'closed', now(), now(), 300)
		ON CONFLICT (workspace_id, provider) DO UPDATE SET state='closed'`, o.WorkspaceID, provider)
	o.Cleanup(t, `DELETE FROM provider_breaker WHERE workspace_id=$1 AND provider=$2`, o.WorkspaceID, provider)
	breaker, err := o.q.GetProviderBreaker(ctx, db.GetProviderBreakerParams{WorkspaceID: parseTestUUID(t, o.WorkspaceID), Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.q.ReleaseProviderHeldTask(ctx, db.ReleaseProviderHeldTaskParams{
		WorkspaceID: parseTestUUID(t, o.WorkspaceID), Provider: provider, ExpectedVersion: breaker.Version, TaskID: parseTestUUID(t, childID),
	}); err != nil {
		t.Fatalf("release: %v", err)
	}
	promote()
	for _, ago := range []string{"1 minute", "9 minutes 59 seconds"} {
		setCompletedAgo(ago)
		if got := claim(); got != nil {
			t.Fatalf("claimed %s %s after the failure, want the 10 minute cooldown", util.UUIDToString(got.ID), ago)
		}
	}

	// 10 minutes and a second after the failure: claimable.
	setCompletedAgo("10 minutes 1 second")
	got := claim()
	if got == nil || util.UUIDToString(got.ID) != childID {
		t.Fatalf("claim after cooldown = %v, want the released retry %s", got, childID)
	}
}
