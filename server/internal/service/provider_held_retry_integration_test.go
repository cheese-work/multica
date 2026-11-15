package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-1736 M1.1: DB-backed proof, through FailTask, of the held retry's
// transaction contract for both provider reasons. Tests only; the retry itself
// is M1.2. A task that fails with a provider 5xx or 429 and whose owner is
// task_retry gets one retry row, its reservation and its receipt in the
// failing transaction. Every refusal or failure of that effect leaves the
// failed status and reason committed and the transaction usable.

// heldRetryReasons is the held path's coverage. 429 left it in CHE-5284 (owner
// option a): it keeps its generic retry until M3b (CHE-1741) releases held rows.
var heldRetryReasons = []string{
	"agent_error.provider_server_error",
}

type heldRetryState struct {
	status, reason string
	children       int
	reservations   int
	receipts       int
	notices        int
}

// failWith fails a running task and reads the committed state back on a
// separate connection. An error from FailTask (ErrTxClosed, aborted
// transaction, a swallowed effect error) is reported as the test failure.
func (o ownerFixture) failWith(t *testing.T, id, reason string) heldRetryState {
	t.Helper()
	ctx := context.Background()
	o.cleanupReceipts(t)
	task, err := o.svc.TaskSvc.FailTask(ctx, parseTestUUID(t, id), "boom", "", "", "", reason, false, "", "")
	if err != nil {
		t.Fatalf("FailTask(%s): %v, want the failure to commit", reason, err)
	}
	if task == nil || task.Status != "failed" {
		t.Fatalf("FailTask returned %+v, want a failed task", task)
	}
	var s heldRetryState
	if err := o.Pool.QueryRow(ctx, `SELECT status, COALESCE(failure_reason, '') FROM agent_task_queue WHERE id=$1`, id).Scan(&s.status, &s.reason); err != nil {
		t.Fatal(err)
	}
	s.children = o.retryChildren(t, id)
	if err := o.Pool.QueryRow(ctx, `SELECT count(*) FROM provider_failure_recovery WHERE failed_task_id=$1`, id).Scan(&s.reservations); err != nil {
		t.Fatal(err)
	}
	s.receipts = len(o.receipts(t, o.Pool))
	s.notices = o.countNotices(t, id)
	return s
}

func (s heldRetryState) wantFailed(t *testing.T, reason string) {
	t.Helper()
	if s.status != "failed" || s.reason != reason {
		t.Fatalf("original task status=%q reason=%q, want failed %q", s.status, s.reason, reason)
	}
}

// Rejects a second effect: no child, no reservation, no receipt.
func (s heldRetryState) wantNoEffect(t *testing.T) {
	t.Helper()
	if s.children != 0 || s.receipts != 0 {
		t.Fatalf("children=%d receipts=%d, want no second recovery effect and no receipt", s.children, s.receipts)
	}
}

func forEachHeldRetryReason(t *testing.T, f func(t *testing.T, o ownerFixture, reason string)) {
	for _, reason := range heldRetryReasons {
		t.Run(strings.TrimPrefix(reason, "agent_error."), func(t *testing.T) {
			f(t, newOwnerFixture(t), reason)
		})
	}
}

// 1 + 3: the held retry commits in the failing transaction, with its
// reservation and its provider_recovery_reserved receipt.
func TestHeldRetryCommitsWithFailureReservationAndReceipt(t *testing.T) {
	forEachHeldRetryReason(t, func(t *testing.T, o ownerFixture, reason string) {
		id := o.runningTask(t, nil)
		s := o.failWith(t, id, reason)
		s.wantFailed(t, reason)
		if s.children != 1 || s.reservations != 1 || s.receipts != 1 {
			t.Fatalf("children=%d reservations=%d receipts=%d, want 1, 1, 1", s.children, s.reservations, s.receipts)
		}
		var child string
		if err := o.Pool.QueryRow(context.Background(),
			`SELECT id FROM agent_task_queue WHERE parent_task_id=$1 OR retry_of_task_id=$1`, id).Scan(&child); err != nil {
			t.Fatal(err)
		}
		var reserved string
		if err := o.Pool.QueryRow(context.Background(),
			`SELECT recovery_task_id::text FROM provider_failure_recovery WHERE failed_task_id=$1`, id).Scan(&reserved); err != nil || reserved != child {
			t.Fatalf("reservation recovery_task_id=%q err=%v, want the retry %s", reserved, err, child)
		}
		r := o.receipts(t, o.Pool)[0]
		if r.Action != providerRecoveryActivityReserved || r.Details["failed_task_id"] != id || r.Details["effect_ref"] != child || r.Details["effect_kind"] != string(providerOwnerTaskRetry) {
			t.Fatalf("receipt=%+v, want %s for failed_task_id=%s effect_ref=%s", r, providerRecoveryActivityReserved, id, child)
		}
		if s.notices != 0 {
			t.Fatalf("owner notices=%d, want 0 for a reserved retry", s.notices)
		}
	})
}

// 1: the retry is created in the failing transaction, so its created_at is at
// or before the failed task's completed_at (both read back from the database).
func TestHeldRetryCreatedAtNotAfterFailedCompletedAt(t *testing.T) {
	forEachHeldRetryReason(t, func(t *testing.T, o ownerFixture, reason string) {
		id := o.runningTask(t, nil)
		o.failWith(t, id, reason)
		var created, completed pgtype.Timestamptz
		err := o.Pool.QueryRow(context.Background(), `
			SELECT c.created_at, p.completed_at
			FROM agent_task_queue p JOIN agent_task_queue c ON c.parent_task_id = p.id OR c.retry_of_task_id = p.id
			WHERE p.id=$1`, id).Scan(&created, &completed)
		if err != nil {
			t.Fatalf("no held retry for the failed task: %v", err)
		}
		if !completed.Valid || created.Time.After(completed.Time) {
			t.Fatalf("retry created_at=%s completed_at=%s, want created_at at or before completed_at", created.Time, completed.Time)
		}
	})
}

// 2: a recovery-lineage task is capped. The failure commits, no second
// recovery effect or receipt appears, and the owner is told once.
func TestHeldRetryCappedLineageKeepsFailureAndAddsNoEffect(t *testing.T) {
	forEachHeldRetryReason(t, func(t *testing.T, o ownerFixture, reason string) {
		id := o.recoveryTask(t, nil)
		s := o.failWith(t, id, reason)
		s.wantFailed(t, reason)
		s.wantNoEffect(t)
		if s.notices != 1 {
			t.Fatalf("owner notices=%d, want 1", s.notices)
		}
	})
}

// 2: a wakeup-bound failure whose wakeup is already in a recovery lineage
// (capped, revision-aware) gets no task retry from this path.
func TestHeldRetryCappedWakeupAddsNoTaskRetry(t *testing.T) {
	forEachHeldRetryReason(t, func(t *testing.T, o ownerFixture, reason string) {
		wakeup := o.wakeup(t, "at", "once", nil)
		o.recordRecovery(t, o.failedTask(t, nil), db.RecordProviderFailureRecoveryParams{
			RecoveryWakeupID: parseTestUUID(t, wakeup), RecoveryWakeupRevision: pgtype.Int8{Int64: 5, Valid: true},
		})
		id := o.runningTask(t, testutil.Cols{"context": testutil.Raw(fmt.Sprintf(`'{"wakeup_id":%q,"wakeup_revision":5}'::jsonb`, wakeup))})
		s := o.failWith(t, id, reason)
		s.wantFailed(t, reason)
		s.wantNoEffect(t)
		if s.reservations != 0 {
			t.Fatalf("reservations=%d, want 0 for a capped wakeup", s.reservations)
		}
	})
}

// 2: a duplicate recovery task. The issue's pending slot is already taken by
// another task, so no second task and no receipt appear and the failure commits.
func TestHeldRetryDuplicateRecoveryTaskKeepsFailureAndAddsNoEffect(t *testing.T) {
	forEachHeldRetryReason(t, func(t *testing.T, o ownerFixture, reason string) {
		id := o.runningTask(t, nil)
		rerun := o.failedTask(t, testutil.Cols{"status": "queued", "failure_reason": nil, "completed_at": nil})
		s := o.failWith(t, id, reason)
		s.wantFailed(t, reason)
		s.wantNoEffect(t)
		var pending int
		if err := o.Pool.QueryRow(context.Background(),
			`SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2 AND status IN ('queued','dispatched','deferred')`,
			o.issueID, o.agentID).Scan(&pending); err != nil || pending != 1 {
			t.Fatalf("pending tasks=%d err=%v, want only the existing %s", pending, err, rerun)
		}
	})
}

// 2: a duplicate recovery run or reservation. A reservation for the failed
// task already exists, so the conflict inserts nothing and the failure commits.
func TestHeldRetryDuplicateReservationKeepsFailureAndAddsNoEffect(t *testing.T) {
	forEachHeldRetryReason(t, func(t *testing.T, o ownerFixture, reason string) {
		id := o.runningTask(t, nil)
		other := o.runningTask(t, nil)
		o.recordRecovery(t, id, db.RecordProviderFailureRecoveryParams{RecoveryTaskID: parseTestUUID(t, other)})
		s := o.failWith(t, id, reason)
		s.wantFailed(t, reason)
		s.wantNoEffect(t)
		if s.reservations != 1 {
			t.Fatalf("reservations=%d, want the original one only", s.reservations)
		}
	})
}

// failingTrigger makes the first matching insert raise, so the recovery
// effect fails in the database rather than through a Go fake.
func (o ownerFixture) failingTrigger(t *testing.T, table, when string) {
	t.Helper()
	name := "held_retry_fail_" + strings.ReplaceAll(strings.ToLower(t.Name()), "/", "_")
	name = strings.NewReplacer("-", "_", " ", "_").Replace(name)
	if len(name) > 55 {
		name = name[len(name)-55:]
	}
	ctx := context.Background()
	for _, sql := range []string{
		fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'held retry test: forced effect failure'; END $$`, name),
		fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON %s FOR EACH ROW WHEN (%s) EXECUTE FUNCTION %s()`, name, table, when, name),
	} {
		if _, err := o.Pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		_, _ = o.Pool.Exec(ctx, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON %s`, name, table))
		_, _ = o.Pool.Exec(ctx, fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, name))
	})
}

// 2: an apply failure. The retry insert itself raises; the savepoint rolls
// back, no reservation or receipt survives, and the failure still commits
// without ErrTxClosed or an aborted-transaction error.
func TestHeldRetryApplyFailureKeepsFailureAndTransactionUsable(t *testing.T) {
	forEachHeldRetryReason(t, func(t *testing.T, o ownerFixture, reason string) {
		id := o.runningTask(t, nil)
		o.failingTrigger(t, "agent_task_queue", fmt.Sprintf(`NEW.parent_task_id = '%s' OR NEW.retry_of_task_id = '%s'`, id, id))
		s := o.failWith(t, id, reason)
		s.wantFailed(t, reason)
		s.wantNoEffect(t)
		if s.reservations != 0 {
			t.Fatalf("reservations=%d, want 0 after a failed apply", s.reservations)
		}
	})
}

// 2: a receipt write failure is an apply failure too: the retry and the
// reservation roll back with it and the failure commits.
func TestHeldRetryReceiptFailureRollsBackEffectAndKeepsFailure(t *testing.T) {
	forEachHeldRetryReason(t, func(t *testing.T, o ownerFixture, reason string) {
		id := o.runningTask(t, nil)
		o.failingTrigger(t, "activity_log", fmt.Sprintf(`NEW.action = '%s' AND NEW.issue_id = '%s'`, providerRecoveryActivityReserved, o.issueID))
		s := o.failWith(t, id, reason)
		s.wantFailed(t, reason)
		s.wantNoEffect(t)
		if s.reservations != 0 {
			t.Fatalf("reservations=%d, want 0 after a failed receipt", s.reservations)
		}
	})
}

// 1 (held contract, M3c): the retry is a deferred row carrying
// context.provider_hold = {provider, since, max_hold_until}, and no promotion
// path flips it to queued before release. A plain dispatchable retry, or a
// queued row with a reservation and receipt, must fail this test.
func TestHeldRetryIsHeldAndCannotDispatchBeforeRelease(t *testing.T) {
	forEachHeldRetryReason(t, func(t *testing.T, o ownerFixture, reason string) {
		ctx := context.Background()
		id := o.runningTask(t, nil)
		s := o.failWith(t, id, reason)
		s.wantFailed(t, reason)
		if s.children != 1 {
			t.Fatalf("children=%d, want one held retry", s.children)
		}
		var child, status, runtimeID string
		var hold []byte
		if err := o.Pool.QueryRow(ctx, `
			SELECT id::text, status, runtime_id::text, COALESCE(context->'provider_hold', 'null'::jsonb)::text::bytea
			FROM agent_task_queue WHERE parent_task_id=$1 OR retry_of_task_id=$1`, id).Scan(&child, &status, &runtimeID, &hold); err != nil {
			t.Fatalf("no held retry for the failed task: %v", err)
		}
		if status != "deferred" {
			t.Fatalf("retry status=%q, want deferred (held)", status)
		}
		var marker map[string]any
		if err := json.Unmarshal(hold, &marker); err != nil || marker == nil {
			t.Fatalf("retry context.provider_hold=%s err=%v, want {provider, since, max_hold_until}", hold, err)
		}
		for _, k := range []string{"provider", "since", "max_hold_until"} {
			if v, ok := marker[k]; !ok || v == nil || v == "" {
				t.Fatalf("provider_hold=%v is missing %q", marker, k)
			}
		}
		// Make the held row due on an online runtime: a due, unmarked deferred
		// row would be promoted, so only the hold keeps this one back.
		if _, err := o.Pool.Exec(ctx, `UPDATE agent_task_queue SET fire_at = now() - interval '1 hour' WHERE id=$1`, child); err != nil {
			t.Fatal(err)
		}
		if _, err := o.Pool.Exec(ctx, `UPDATE agent_runtime SET status='online', last_seen_at=now() WHERE id=$1`, runtimeID); err != nil {
			t.Fatal(err)
		}
		promoted, err := db.New(o.Pool).PromoteDueDeferredTasksForRuntime(ctx, db.PromoteDueDeferredTasksForRuntimeParams{
			RuntimeID: parseTestUUID(t, runtimeID), RuntimeStaleSecs: 3600,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range promoted {
			if util.UUIDToString(p.ID) == child {
				t.Fatalf("PromoteDueDeferredTasksForRuntime promoted held retry %s, want it held until release", child)
			}
		}
		if err := o.Pool.QueryRow(ctx, `SELECT status FROM agent_task_queue WHERE id=$1`, child).Scan(&status); err != nil || status != "deferred" {
			t.Fatalf("held retry status=%q err=%v after a due promotion tick, want deferred", status, err)
		}
	})
}

// Owner option (a), CHE-5284: until M3b (CHE-1741) can release a held row, a
// 429 keeps the generic 10-minute retry. It is a plain deferred row: no
// provider_hold marker (the promotion queries would skip it), no reservation,
// no receipt.
func TestHeldRetryLeavesRateLimitOnGenericRetry(t *testing.T) {
	const reason = "agent_error.provider_capacity_or_rate_limit"
	o := newOwnerFixture(t)
	id := o.runningTask(t, nil)
	s := o.failWith(t, id, reason)
	s.wantFailed(t, reason)
	if s.children != 1 || s.reservations != 0 || s.receipts != 0 {
		t.Fatalf("children=%d reservations=%d receipts=%d, want 1, 0, 0", s.children, s.reservations, s.receipts)
	}
	var status string
	var held bool
	if err := o.Pool.QueryRow(context.Background(),
		`SELECT status, COALESCE(context ? 'provider_hold', false) FROM agent_task_queue WHERE parent_task_id=$1 OR retry_of_task_id=$1`, id).Scan(&status, &held); err != nil {
		t.Fatalf("no generic retry for the 429: %v", err)
	}
	if held || status != "deferred" {
		t.Fatalf("429 retry status=%q provider_hold=%t, want a deferred row without the marker", status, held)
	}
}

// CHE-1736 Design: the held retry runs only while the issue is still assigned
// to the failed task's agent or squad. A person who unassigned the issue while
// the run was failing gets no held retry, no reservation and no receipt.
func TestHeldRetryUnassignedIssueGetsNoHeldRetry(t *testing.T) {
	o := newOwnerFixture(t)
	id := o.runningTask(t, nil)
	if _, err := o.Pool.Exec(context.Background(), `UPDATE issue SET assignee_type=NULL, assignee_id=NULL WHERE id=$1`, o.issueID); err != nil {
		t.Fatal(err)
	}
	s := o.failWith(t, id, "agent_error.provider_server_error")
	s.wantFailed(t, "agent_error.provider_server_error")
	if s.children != 0 || s.reservations != 0 || s.receipts != 0 {
		t.Fatalf("children=%d reservations=%d receipts=%d, want 0, 0, 0 for an unassigned issue", s.children, s.reservations, s.receipts)
	}
}
