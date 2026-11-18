package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-1754 (CHE-1732 M1b): the one held retry for a failed run that is
// wakeup-bound and not owned by the recurring guard: runs of continuous
// kind=event wakeups and of system-rule wakeups. The retry carries the
// wakeup's id and revision, is fenced on the wakeup at creation and at release,
// and keeps M0's cap, receipt and one-notice rules.

// wakeupRunContext is the context of a run fired by wakeup at revision.
func wakeupRunContext(t *testing.T, wakeupID string, revision int) testutil.Cols {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"wakeup_id": wakeupID, "wakeup_revision": revision})
	if err != nil {
		t.Fatal(err)
	}
	return testutil.Cols{"context": testutil.Raw(fmt.Sprintf("'%s'::jsonb", payload))}
}

// m1bWakeups are the wakeups M1b covers. The recurring every/cron guard and
// the once-mode re-arm own the rest.
var m1bWakeups = []struct {
	name, kind, mode string
	rule             any
}{
	{"continuous event", "event", "continuous", nil},
	{"system rule", "event", "continuous", "child_done"},
}

func forEachM1bWakeup(t *testing.T, f func(t *testing.T, o ownerFixture, wakeupID string)) {
	for _, w := range m1bWakeups {
		t.Run(w.name, func(t *testing.T) {
			o := newOwnerFixture(t)
			f(t, o, o.wakeup(t, w.kind, w.mode, w.rule))
		})
	}
}

func (o ownerFixture) heldChild(t *testing.T, failed string) (id, status string, ctxJSON map[string]any) {
	t.Helper()
	var raw []byte
	if err := o.Pool.QueryRow(context.Background(),
		`SELECT id::text, status, context::text::bytea FROM agent_task_queue WHERE parent_task_id=$1 OR retry_of_task_id=$1`, failed).Scan(&id, &status, &raw); err != nil {
		t.Fatalf("no held retry for %s: %v", failed, err)
	}
	if err := json.Unmarshal(raw, &ctxJSON); err != nil {
		t.Fatal(err)
	}
	return id, status, ctxJSON
}

// One held retry, its reservation and its receipt, bound to the same wakeup
// and revision, for each covered wakeup.
func TestM1bWakeupRunGetsOneHeldRetry(t *testing.T) {
	forEachM1bWakeup(t, func(t *testing.T, o ownerFixture, wakeup string) {
		for _, reason := range heldRetryReasons {
			t.Run(reason, func(t *testing.T) {
				id := o.runningTask(t, wakeupRunContext(t, wakeup, 1))
				s := o.failWith(t, id, reason)
				s.wantFailed(t, reason)
				if s.children != 1 || s.reservations != 1 || s.receipts != 1 || s.notices != 0 {
					t.Fatalf("children=%d reservations=%d receipts=%d notices=%d, want 1, 1, 1, 0", s.children, s.reservations, s.receipts, s.notices)
				}
				child, status, c := o.heldChild(t, id)
				if status != "deferred" || c["provider_hold"] == nil || c["wakeup_id"] != wakeup || c["wakeup_revision"] != float64(1) {
					t.Fatalf("child status=%q context=%v, want a held deferred row bound to wakeup %s revision 1", status, c, wakeup)
				}
				if r := o.receipts(t, o.Pool)[0]; r.Details["failed_task_id"] != id || r.Details["effect_ref"] != child || r.Details["effect_kind"] != string(providerOwnerTaskRetry) {
					t.Fatalf("receipt=%+v, want effect_ref=%s for failed_task_id=%s", r, child, id)
				}
			})
		}
	})
}

// The recurring guard's wakeups and schedule autopilots keep their own recovery.
func TestM1bGuardOwnedWakeupGetsNoHeldRetry(t *testing.T) {
	for _, kind := range []string{"every", "cron"} {
		t.Run(kind, func(t *testing.T) {
			o := newOwnerFixture(t)
			s := o.failWith(t, o.runningTask(t, wakeupRunContext(t, o.wakeup(t, kind, "continuous", nil), 1)), held5xx)
			s.wantFailed(t, held5xx)
			s.wantNoEffect(t)
			if s.reservations != 0 || s.notices != 0 {
				t.Fatalf("reservations=%d notices=%d, want 0, 0", s.reservations, s.notices)
			}
		})
	}
}

// Creation fence: a wakeup that is gone, disabled, edited or paused when the
// run fails gets no retry, no reservation and no receipt.
func TestM1bCreationFence(t *testing.T) {
	for name, mutate := range map[string]string{
		"deleted":          `DELETE FROM issue_wakeup WHERE id=$1`,
		"disabled":         `UPDATE issue_wakeup SET disabled_at=now(), enabled=false WHERE id=$1`,
		"edited":           `UPDATE issue_wakeup SET revision=revision+1 WHERE id=$1`,
		"workspace paused": `UPDATE workspace SET settings=COALESCE(settings,'{}'::jsonb)||'{"wakeups_paused":true}'::jsonb WHERE id=(SELECT workspace_id FROM issue_wakeup WHERE id=$1)`,
	} {
		t.Run(name, func(t *testing.T) {
			o := newOwnerFixture(t)
			wakeup := o.wakeup(t, "event", "continuous", nil)
			id := o.runningTask(t, wakeupRunContext(t, wakeup, 1))
			o.Cleanup(t, `UPDATE workspace SET settings=settings-'wakeups_paused' WHERE id=$1`, o.WorkspaceID)
			o.Exec(t, mutate, wakeup)
			s := o.failWith(t, id, held5xx)
			s.wantFailed(t, held5xx)
			s.wantNoEffect(t)
			if s.reservations != 0 || s.notices != 0 {
				t.Fatalf("reservations=%d notices=%d, want 0, 0", s.reservations, s.notices)
			}
		})
	}
}

// heldWakeupRetry fails a wakeup-bound run on a known provider and returns the
// held child. The failing transaction also writes the closed breaker row.
func (o ownerFixture) heldWakeupRetry(t *testing.T, wakeup string) string {
	t.Helper()
	o.Exec(t, `UPDATE agent SET model='gpt-5.5' WHERE id=$1`, o.agentID)
	id := o.runningTask(t, wakeupRunContext(t, wakeup, 1))
	if s := o.failWith(t, id, held5xx); s.children != 1 {
		t.Fatalf("children=%d, want one held retry", s.children)
	}
	o.Cleanup(t, `DELETE FROM provider_breaker WHERE workspace_id=$1`, o.WorkspaceID)
	child, _, _ := o.heldChild(t, id)
	return child
}

func (o ownerFixture) release(t *testing.T) {
	t.Helper()
	if err := o.svc.TaskSvc.ReleaseProviderHeldTasks(context.Background(), time.Now().Add(providerCapacityRetryWait+time.Minute)); err != nil {
		t.Fatal(err)
	}
}

func (o ownerFixture) taskState(t *testing.T, id string) (status, reason string, held bool) {
	t.Helper()
	if err := o.Pool.QueryRow(context.Background(),
		`SELECT status, COALESCE(failure_reason, ''), context ? 'provider_hold' FROM agent_task_queue WHERE id=$1`, id).Scan(&status, &reason, &held); err != nil {
		t.Fatal(err)
	}
	return
}

// Release fence: the wakeup disabled, deleted or edited, or the workspace
// paused (CHE-1727 wakeups_paused), between failure and release cancels the
// held row as wakeup_superseded. A pause is not replayed after the resume.
func TestM1bReleaseFence(t *testing.T) {
	const pause = `UPDATE workspace SET settings=COALESCE(settings,'{}'::jsonb)||'{"wakeups_paused":true}'::jsonb WHERE id=$2 AND $1::text IS NOT NULL`
	for _, tc := range []struct {
		name, mutate string
		superseded   bool
	}{
		{"unchanged", `UPDATE issue_wakeup SET updated_at=updated_at WHERE id=$1 AND $2::text IS NOT NULL`, false},
		{"deleted", `DELETE FROM issue_wakeup WHERE id=$1 AND $2::text IS NOT NULL`, true},
		{"disabled", `UPDATE issue_wakeup SET disabled_at=now(), enabled=false WHERE id=$1 AND $2::text IS NOT NULL`, true},
		{"edited", `UPDATE issue_wakeup SET revision=revision+1 WHERE id=$1 AND $2::text IS NOT NULL`, true},
		{"workspace paused", pause, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newOwnerFixture(t)
			wakeup := o.wakeup(t, "event", "continuous", nil)
			child := o.heldWakeupRetry(t, wakeup)
			o.Cleanup(t, `UPDATE workspace SET settings=settings-'wakeups_paused' WHERE id=$1`, o.WorkspaceID)
			o.Exec(t, tc.mutate, wakeup, o.WorkspaceID)
			o.release(t)
			status, reason, held := o.taskState(t, child)
			if tc.superseded && (status != "cancelled" || reason != "wakeup_superseded") {
				t.Fatalf("status=%q reason=%q, want cancelled wakeup_superseded", status, reason)
			}
			if !tc.superseded && (held || status != "deferred") {
				t.Fatalf("status=%q held=%v, want released and still deferred", status, held)
			}
			if tc.name == "workspace paused" {
				o.Exec(t, `UPDATE workspace SET settings=settings-'wakeups_paused' WHERE id=$1`, o.WorkspaceID)
				o.release(t)
				if status, _, _ := o.taskState(t, child); status != "cancelled" {
					t.Fatalf("status=%q after resume, want the cancelled row not replayed", status)
				}
			}
			if n := o.countNotices(t, child); n != 0 {
				t.Fatalf("owner notices=%d for a superseded or released row, want 0", n)
			}
		})
	}
}

// The retry's own failure creates no reservation and no task and sends one
// owner notice, whether the wakeup is unchanged or not. A generic retry reason
// with budget left adds no child either (M0d).
func TestM1bRetryFailureMintsNothingAndNotifiesOnce(t *testing.T) {
	for _, reason := range []string{held5xx, "agent_error.timeout", "agent_error.provider_network"} {
		t.Run(reason, func(t *testing.T) {
			o := newOwnerFixture(t)
			wakeup := o.wakeup(t, "event", "continuous", nil)
			first := o.runningTask(t, wakeupRunContext(t, wakeup, 1))
			if s := o.failWith(t, first, held5xx); s.children != 1 {
				t.Fatalf("first failure children=%d, want one held retry", s.children)
			}
			child, _, _ := o.heldChild(t, first)
			o.Exec(t, `UPDATE agent_task_queue SET status='running', fire_at=NULL, max_attempts=3 WHERE id=$1`, child)
			s := o.failWith(t, child, reason)
			if s.children != 0 || s.reservations != 0 {
				t.Fatalf("children=%d reservations=%d, want 0, 0", s.children, s.reservations)
			}
			if reason == held5xx && s.notices != 1 {
				t.Fatalf("notices=%d, want 1", s.notices)
			}
		})
	}
}

// Claim cooldown (CHE-1699), fake clock: a released wakeup-bound retry is not
// claimed until 10 minutes after the failed task's completed_at.
func TestM1bClaimWaitsForGatewayCooldownAfterRelease(t *testing.T) {
	o := newOwnerFixture(t)
	ctx := context.Background()
	wakeup := o.wakeup(t, "event", "continuous", nil)
	child := o.heldWakeupRetry(t, wakeup)
	failed := o.mustParent(t, child)
	var runtimeID string
	if err := o.Pool.QueryRow(ctx, `SELECT runtime_id::text FROM agent_task_queue WHERE id=$1`, child).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	o.Exec(t, `UPDATE agent_runtime SET status='online', last_seen_at=now() WHERE id=$1`, runtimeID)
	o.release(t)
	if _, _, held := o.taskState(t, child); held {
		t.Fatal("retry still held after release")
	}
	promote := func() {
		t.Helper()
		if _, err := o.q.PromoteDueDeferredTasksForRuntime(ctx, db.PromoteDueDeferredTasksForRuntimeParams{
			RuntimeID: parseTestUUID(t, runtimeID), RuntimeStaleSecs: 3600,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Fake clock: ClaimTask reads time.Now(), so "time since the failure" is
	// moved by setting completed_at to an instant computed in Go, the same
	// pattern as the CHE-1699 tests in gateway_recovery_test.go. Database now()
	// is not used.
	claimAfter := func(elapsed time.Duration) *db.AgentTaskQueue {
		t.Helper()
		failedAt := time.Now().Add(-elapsed)
		o.Exec(t, `UPDATE agent_task_queue SET completed_at = $2 WHERE id=$1`, failed, pgtype.Timestamptz{Time: failedAt, Valid: true})
		o.Exec(t, `UPDATE agent_task_queue SET fire_at = $2 WHERE id=$1`, child, pgtype.Timestamptz{Time: failedAt, Valid: true})
		promote()
		got, err := o.svc.TaskSvc.ClaimTask(ctx, parseTestUUID(t, o.agentID))
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	// Margins of 1 min before and 30 s after the 10 min edge keep a slow run from crossing it.
	for _, elapsed := range []time.Duration{time.Minute, gatewayFailureCooldown - time.Minute} {
		if got := claimAfter(elapsed); got != nil {
			t.Fatalf("claimed %s %s after the failure, want the %s cooldown", util.UUIDToString(got.ID), elapsed, gatewayFailureCooldown)
		}
	}
	if got := claimAfter(gatewayFailureCooldown + 30*time.Second); got == nil || util.UUIDToString(got.ID) != child {
		t.Fatalf("claim after cooldown = %v, want the released retry %s", got, child)
	}
}

// Rollback receipt: when the receipt write fails, the failing transaction rolls
// back the held row and its reservation with it. The failure itself commits and
// none of the three exist. (The commit-together case is
// TestM1bWakeupRunGetsOneHeldRetry: children, reservations and receipts are 1.)
func TestM1bReceiptFailureRollsBackHeldRowAndReservation(t *testing.T) {
	forEachM1bWakeup(t, func(t *testing.T, o ownerFixture, wakeup string) {
		for _, reason := range heldRetryReasons {
			t.Run(strings.TrimPrefix(reason, "agent_error."), func(t *testing.T) {
				id := o.runningTask(t, wakeupRunContext(t, wakeup, 1))
				o.failingTrigger(t, "activity_log", fmt.Sprintf(`NEW.action = '%s' AND NEW.issue_id = '%s'`, providerRecoveryActivityReserved, o.issueID))
				s := o.failWith(t, id, reason)
				s.wantFailed(t, reason)
				s.wantNoEffect(t)
				if s.reservations != 0 {
					t.Fatalf("reservations=%d, want 0 after a failed receipt", s.reservations)
				}
			})
		}
	})
}

// Restart: the held row, its wakeup binding and its hold marker live in the
// database. A fresh TaskService (a process restart) finds the row still held,
// and its release lets the retry through.
func TestM1bRestartKeepsHeldRow(t *testing.T) {
	forEachM1bWakeup(t, func(t *testing.T, o ownerFixture, wakeup string) {
		child := o.heldWakeupRetry(t, wakeup)
		restarted := NewTaskService(db.New(o.Pool), o.Pool, nil, events.New())
		if status, _, held := o.taskState(t, child); status != "deferred" || !held {
			t.Fatalf("before release: status=%q held=%v, want a held deferred row", status, held)
		}
		if err := restarted.ReleaseProviderHeldTasks(context.Background(), time.Now().Add(providerCapacityRetryWait+time.Minute)); err != nil {
			t.Fatal(err)
		}
		status, reason, held := o.taskState(t, child)
		if held || status == "cancelled" || reason == "wakeup_superseded" {
			t.Fatalf("after release by the restarted service: status=%q reason=%q held=%v, want the retry released and alive", status, reason, held)
		}
		_, _, c := o.heldChild(t, o.mustParent(t, child))
		if c["wakeup_id"] != wakeup || c["wakeup_revision"] != float64(1) {
			t.Fatalf("released retry context=%v, want wakeup %s revision 1 kept", c, wakeup)
		}
	})
}

// M0d cap, post-re-arm: a run of the wakeup at a revision already in a
// recovery lineage that fails with a held or a generic retry reason, with
// attempt budget left, gets no retry of any kind.
func TestM1bPostRearmRunGetsNoRetryChild(t *testing.T) {
	forEachM1bWakeup(t, func(t *testing.T, o ownerFixture, wakeup string) {
		o.recordRecovery(t, o.failedTask(t, nil), db.RecordProviderFailureRecoveryParams{
			RecoveryWakeupID: parseTestUUID(t, wakeup), RecoveryWakeupRevision: pgtype.Int8{Int64: 5, Valid: true},
		})
		for _, reason := range []string{held5xx, "agent_error.provider_capacity_or_rate_limit", "agent_error.timeout", "agent_error.provider_network"} {
			t.Run(reason, func(t *testing.T) {
				id := o.runningTask(t, wakeupRunContext(t, wakeup, 5))
				s := o.failWith(t, id, reason)
				s.wantFailed(t, reason)
				s.wantNoEffect(t)
				if s.reservations != 0 {
					t.Fatalf("reservations=%d, want 0 for a wakeup already in a recovery lineage", s.reservations)
				}
			})
		}
	})
}

func (o ownerFixture) mustParent(t *testing.T, child string) string {
	t.Helper()
	var parent string
	if err := o.Pool.QueryRow(context.Background(), `SELECT COALESCE(retry_of_task_id, parent_task_id)::text FROM agent_task_queue WHERE id=$1`, child).Scan(&parent); err != nil {
		t.Fatal(err)
	}
	return parent
}
