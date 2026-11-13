package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// CHE-1741 M3b: the evidence-gated release controller. Fake clock: every tick
// passes its own `now`, so no test sleeps and none calls live inference.

const pse = "agent_error.provider_server_error"

type releaseRig struct {
	*breakerRig
	svc  *TaskService
	base time.Time
}

func newReleaseRig(t *testing.T) *releaseRig {
	t.Helper()
	r := newBreakerRig(t)
	return &releaseRig{breakerRig: r, svc: newBreakerSvc(r), base: time.Now().UTC().Truncate(time.Second)}
}

// newBreakerSvc builds a fresh service: a controller restart keeps no memory.
func newBreakerSvc(r *breakerRig) *TaskService {
	return NewTaskService(db.New(r.pool), r.pool, nil, events.New())
}

func (r *releaseRig) x(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := r.pool.Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// closedBreaker sets a closed breaker; lastSuccess is the zero time for none.
func (r *releaseRig) closedBreaker(t *testing.T, lastSuccess time.Time) {
	t.Helper()
	r.breaker(t, "closed", time.Hour, 300, pgtype.UUID{})
	if !lastSuccess.IsZero() {
		r.x(t, `UPDATE provider_breaker SET last_success_at=$2 WHERE workspace_id=$1 AND provider='openai'`, r.workspaceID, lastSuccess)
	}
}

// heldRow is a held retry row: deferred, context.provider_hold set, and, when
// failedAgo > 0, the retry of a task that failed that long before r.base.
func (r *releaseRig) heldRow(t *testing.T, since time.Time, failedAgo time.Duration, extra string) pgtype.UUID {
	t.Helper()
	id := r.task(t, "queued", "", 0)
	if failedAgo > 0 {
		parent := r.task(t, "failed", pse, 0)
		r.x(t, `UPDATE agent_task_queue SET completed_at=$2 WHERE id=$1`, parent, r.base.Add(-failedAgo))
		r.x(t, `UPDATE agent_task_queue SET retry_of_task_id=$2 WHERE id=$1`, id, parent)
	}
	hold := fmt.Sprintf(`{"provider_hold":{"provider":"openai","since":%q,"max_hold_until":"2099-01-01T00:00:00Z"}%s}`, since.Format(time.RFC3339), extra)
	r.x(t, `UPDATE agent_task_queue SET status='deferred', fire_at=$2, created_at=$3, context=$4::jsonb WHERE id=$1`,
		id, r.base.Add(-time.Hour), since, hold)
	return id
}

// heldRows makes n held retries of tasks that failed an hour before base.
func (r *releaseRig) heldRows(t *testing.T, n int) []pgtype.UUID {
	t.Helper()
	ids := make([]pgtype.UUID, n)
	for i := range ids {
		ids[i] = r.heldRow(t, r.base.Add(-time.Hour+time.Duration(i)*time.Second), time.Hour, "")
	}
	return ids
}

func (r *releaseRig) tick(t *testing.T, at time.Duration) {
	t.Helper()
	if err := r.svc.ReleaseProviderHeldTasks(context.Background(), r.base.Add(at)); err != nil {
		t.Fatalf("ReleaseProviderHeldTasks(%v): %v", at, err)
	}
}

// released reports which rows lost their hold and were not superseded.
func (r *releaseRig) released(t *testing.T, ids []pgtype.UUID) []pgtype.UUID {
	t.Helper()
	var out []pgtype.UUID
	for _, id := range ids {
		var held bool
		var reason string
		if err := r.pool.QueryRow(context.Background(),
			`SELECT context ? 'provider_hold', COALESCE(failure_reason, '') FROM agent_task_queue WHERE id=$1`, id).Scan(&held, &reason); err != nil {
			t.Fatal(err)
		}
		if !held && reason != "wakeup_superseded" {
			out = append(out, id)
		}
	}
	return out
}

func (r *releaseRig) wantReleased(t *testing.T, when string, ids []pgtype.UUID, want int) []pgtype.UUID {
	t.Helper()
	got := r.released(t, ids)
	if len(got) != want {
		t.Fatalf("%s: %d rows released, want %d", when, len(got), want)
	}
	return got
}

// settle ends a released canary with a terminal outcome at base+at.
func (r *releaseRig) settle(t *testing.T, id pgtype.UUID, status, reason string, at time.Duration) {
	t.Helper()
	r.x(t, `UPDATE agent_task_queue SET status=$2, failure_reason=NULLIF($3, ''), completed_at=$4 WHERE id=$1`, id, status, reason, r.base.Add(at))
}

func (r *releaseRig) breakerState(t *testing.T) string {
	t.Helper()
	state, _, _, ok := r.row(t)
	if !ok {
		t.Fatal("no provider_breaker row")
	}
	return state
}

// No release by timer alone: with no evidence and the canary slot taken,
// nothing is released however long the clock runs.
func TestReleaseControllerNoReleaseByTimerAlone(t *testing.T) {
	r := newReleaseRig(t)
	r.closedBreaker(t, time.Time{})
	canary := r.task(t, "queued", "", 0)
	r.x(t, `UPDATE agent_task_queue SET status='running' WHERE id=$1`, canary)
	r.x(t, `UPDATE provider_breaker SET canary_task_id=$2 WHERE workspace_id=$1 AND provider='openai'`, r.workspaceID, canary)
	ids := r.heldRows(t, 3)
	for _, at := range []time.Duration{0, time.Hour, 24 * time.Hour, 100 * time.Hour} {
		r.tick(t, at)
		r.wantReleased(t, fmt.Sprintf("tick +%v", at), ids, 0)
	}
}

// Evidence is a success newer than the row's failed_at; release is staggered
// at 2 starts per 15 s. A success older than failed_at is no evidence.
func TestReleaseControllerEvidenceReleasesStaggered(t *testing.T) {
	r := newReleaseRig(t)
	r.closedBreaker(t, r.base.Add(-time.Minute))
	ids := r.heldRows(t, 5)
	for _, step := range []struct {
		at   time.Duration
		want int
	}{{0, 2}, {5 * time.Second, 2}, {14 * time.Second, 2}, {15 * time.Second, 4}, {20 * time.Second, 4}, {30 * time.Second, 5}} {
		r.tick(t, step.at)
		r.wantReleased(t, fmt.Sprintf("tick +%v", step.at), ids, step.want)
	}
}

func TestReleaseControllerStaleSuccessIsNotEvidence(t *testing.T) {
	r := newReleaseRig(t)
	r.closedBreaker(t, r.base.Add(-2*time.Hour)) // before the rows' failed_at (-1h)
	ids := r.heldRows(t, 4)
	r.tick(t, 0)
	r.wantReleased(t, "stale success: canary only", ids, 1)
	r.tick(t, 15*time.Second)
	r.wantReleased(t, "stale success, no stagger release", ids, 1)
}

// One canary at a time, also across a controller restart.
func TestReleaseControllerOneCanaryAcrossRestart(t *testing.T) {
	r := newReleaseRig(t)
	r.closedBreaker(t, time.Time{})
	ids := r.heldRows(t, 3)
	r.tick(t, 0)
	first := r.wantReleased(t, "first tick", ids, 1)
	r.svc = newBreakerSvc(r.breakerRig) // restart: state lives in the DB only
	r.tick(t, 30*time.Minute)
	r.tick(t, time.Hour)
	if got := r.wantReleased(t, "after restart", ids, 1); got[0] != first[0] {
		t.Fatalf("canary changed across restart")
	}
	var canary pgtype.UUID
	r.pool.QueryRow(context.Background(), `SELECT canary_task_id FROM provider_breaker WHERE workspace_id=$1 AND provider='openai'`, r.workspaceID).Scan(&canary)
	if canary != first[0] {
		t.Fatalf("canary_task_id=%v, want the released row", canary)
	}
}

// A canary success sets last_success_at, after which R1 releases the rest.
func TestReleaseControllerCanarySuccessThenEvidence(t *testing.T) {
	r := newReleaseRig(t)
	r.closedBreaker(t, time.Time{})
	ids := r.heldRows(t, 4)
	r.tick(t, 0)
	canary := r.wantReleased(t, "canary", ids, 1)[0]
	r.tick(t, 2*time.Minute)
	r.wantReleased(t, "canary still running", ids, 1)

	r.settle(t, canary, "completed", "", time.Minute)
	if err := recordProviderSuccess(context.Background(), db.New(r.pool), db.AgentTaskQueue{ID: canary, AgentID: r.agentID, Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	r.tick(t, 3*time.Minute)
	r.wantReleased(t, "evidence tick 1", ids, 3)
	r.tick(t, 3*time.Minute+15*time.Second)
	r.wantReleased(t, "evidence tick 2", ids, 4)
}

// A failed canary leaves the evidence missing and doubles the cool-off:
// 5 min, then 10, 20, and 30 min as the cap.
func TestReleaseControllerCanaryFailureDoublesCoolOff(t *testing.T) {
	r := newReleaseRig(t)
	r.closedBreaker(t, time.Time{})
	ids := r.heldRows(t, 8)
	next := func(at time.Duration, n int) pgtype.UUID {
		t.Helper()
		r.tick(t, at)
		return r.wantReleased(t, fmt.Sprintf("+%v", at), ids, n)[n-1]
	}
	// Each step: release at `at`, the canary fails 30 s later, the controller
	// notices 1 s after that. Nothing may start before `quiet`; one starts by `due`.
	steps := []struct{ at, quiet, due time.Duration }{
		{0, 9 * time.Minute, 12 * time.Minute},                // 5 -> 10 min
		{12 * time.Minute, 31 * time.Minute, 34 * time.Minute}, // 10 -> 20 min
		{34 * time.Minute, 63 * time.Minute, 66 * time.Minute}, // 20 -> 30 min
		{66 * time.Minute, 95 * time.Minute, 98 * time.Minute}, // capped at 30, not 40
	}
	canary := next(0, 1)
	for i, s := range steps {
		r.settle(t, canary, "failed", pse, s.at+30*time.Second)
		r.tick(t, s.at+31*time.Second)
		r.wantReleased(t, fmt.Sprintf("step %d after failure", i), ids, i+1)
		r.tick(t, s.quiet)
		r.wantReleased(t, fmt.Sprintf("step %d inside cool-off", i), ids, i+1)
		canary = next(s.due, i+2)
	}
}

// At most 6 canaries per hour, even when every canary ends without a verdict.
func TestReleaseControllerHourlyCanaryCap(t *testing.T) {
	r := newReleaseRig(t)
	r.closedBreaker(t, time.Time{})
	ids := r.heldRows(t, 9)
	canary := pgtype.UUID{}
	for k := 0; k <= 12; k++ {
		at := time.Duration(k) * 301 * time.Second
		if canary.Valid {
			r.settle(t, canary, "cancelled", "", at-time.Second)
		}
		r.tick(t, at)
		got := r.released(t, ids)
		want := k + 1
		if k >= 6 && k < 12 {
			want = 6
		} else if k == 12 {
			want = 7
		}
		if len(got) != want {
			t.Fatalf("k=%d (+%v): %d canaries released, want %d", k, at, len(got), want)
		}
		if len(got) > 0 {
			canary = got[len(got)-1]
		}
	}
}

func (r *releaseRig) recoveryRetry(t *testing.T, id pgtype.UUID) {
	t.Helper()
	failed := r.task(t, "failed", pse, time.Hour)
	r.x(t, `INSERT INTO provider_failure_recovery (failed_task_id, trigger_kind, trigger_id, condition_key, failed_at, changed_condition, recovery_task_id)
		VALUES ($1, 'issue_wakeup', $2, '', now(), 'recovery_reserved', $3)`, failed, dbid.NewV7(), id)
}

// A canary prefers a non-recovery row; the oldest recovery retry only when
// nothing else is held.
func TestReleaseControllerCanaryPrefersNonRecoveryRow(t *testing.T) {
	r := newReleaseRig(t)
	r.closedBreaker(t, time.Time{})
	oldRecovery := r.heldRow(t, r.base.Add(-2*time.Hour), time.Hour, "")
	r.recoveryRetry(t, oldRecovery)
	plain := r.heldRow(t, r.base.Add(-time.Hour), time.Hour, "")
	r.tick(t, 0)
	got := r.wantReleased(t, "mixed", []pgtype.UUID{oldRecovery, plain}, 1)
	if got[0] != plain {
		t.Fatalf("canary = the recovery retry, want the non-recovery row")
	}

	sole := newReleaseRig(t)
	sole.closedBreaker(t, time.Time{})
	a := sole.heldRow(t, sole.base.Add(-2*time.Hour), time.Hour, "")
	b := sole.heldRow(t, sole.base.Add(-time.Hour), time.Hour, "")
	sole.recoveryRetry(t, a)
	sole.recoveryRetry(t, b)
	sole.tick(t, 0)
	if got := sole.wantReleased(t, "only recovery retries", []pgtype.UUID{a, b}, 1); got[0] != a {
		t.Fatalf("sole-recovery canary is not the oldest recovery retry")
	}
}

// open -> half_open -> draining -> closed, with an arrival held until closed.
func TestReleaseControllerOpenProbeDrainsThenCloses(t *testing.T) {
	r := newReleaseRig(t)
	r.breaker(t, "open", time.Hour, 300, pgtype.UUID{})
	ids := r.heldRows(t, 3)
	r.tick(t, 0)
	r.wantReleased(t, "open, probe not due", ids, 0)
	if s := r.breakerState(t); s != "open" {
		t.Fatalf("state %q before next_probe_at, want open", s)
	}

	r.x(t, `UPDATE provider_breaker SET next_probe_at=$2 WHERE workspace_id=$1 AND provider='openai'`, r.workspaceID, r.base.Add(-time.Second))
	r.tick(t, 0)
	canary := r.wantReleased(t, "probe due", ids, 1)[0]
	if s := r.breakerState(t); s != "half_open" {
		t.Fatalf("state %q after the probe starts, want half_open", s)
	}
	r.tick(t, 20*time.Minute)
	r.wantReleased(t, "half_open waits on its canary", ids, 1)

	arrival := r.heldRow(t, r.base.Add(time.Hour), 0, "")
	all := append(append([]pgtype.UUID{}, ids...), arrival)
	r.settle(t, canary, "completed", "", time.Minute)
	if err := recordProviderSuccess(context.Background(), db.New(r.pool), db.AgentTaskQueue{ID: canary, AgentID: r.agentID, Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	if s := r.breakerState(t); s != "draining" {
		t.Fatalf("state %q after canary success with a backlog, want draining", s)
	}
	closed := false
	for i := 1; i <= 8; i++ {
		r.tick(t, time.Duration(i)*20*time.Second+time.Hour)
		state := r.breakerState(t)
		if state != "closed" {
			if rel := r.released(t, []pgtype.UUID{arrival}); len(rel) != 0 {
				t.Fatalf("arrival released while %s", state)
			}
		} else {
			closed = true
		}
	}
	if !closed {
		t.Fatalf("breaker never closed after the backlog drained")
	}
	r.wantReleased(t, "after close", all, len(all))
}

// half_open -> open: the canary fails, the cool-off doubles.
func TestReleaseControllerHalfOpenFailureReopensDoubled(t *testing.T) {
	r := newReleaseRig(t)
	r.breaker(t, "open", -time.Second, 600, pgtype.UUID{})
	ids := r.heldRows(t, 2)
	r.tick(t, 0)
	canary := r.wantReleased(t, "probe", ids, 1)[0]
	r.settle(t, canary, "failed", pse, time.Minute)
	r.fail(t, canary)
	state, coolOff, c, _ := r.row(t)
	if state != "open" || coolOff != 1200 || c.Valid {
		t.Fatalf("after canary failure: state %q cool_off %d canary %v, want open, 1200, none", state, coolOff, c)
	}
	r.tick(t, 2*time.Minute)
	r.wantReleased(t, "reopened breaker", ids, 1)
}

// Unknown state releases nothing: no breaker row, or a read that errors.
func TestReleaseControllerUnknownStateReleasesNothing(t *testing.T) {
	r := newReleaseRig(t)
	ids := r.heldRows(t, 2)
	r.tick(t, time.Hour) // no provider_breaker row
	r.wantReleased(t, "no breaker row", ids, 0)

	r.closedBreaker(t, r.base.Add(-time.Minute))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = r.svc.ReleaseProviderHeldTasks(ctx, r.base)
	r.wantReleased(t, "cancelled context", ids, 0)
}

// A human hold on the provider blocks release in any state.
func TestReleaseControllerHumanHoldBlocksRelease(t *testing.T) {
	r := newReleaseRig(t)
	r.closedBreaker(t, r.base.Add(-time.Minute))
	ids := r.heldRows(t, 2)
	r.x(t, `UPDATE workspace SET settings = jsonb_build_object('provider_holds', jsonb_build_array(jsonb_build_object('provider', 'OpenAI', 'text', 'paused by hand'))) WHERE id=$1`, r.workspaceID)
	r.tick(t, 0)
	r.tick(t, time.Hour)
	r.wantReleased(t, "human hold", ids, 0)
	r.x(t, `UPDATE workspace SET settings = '{}'::jsonb WHERE id=$1`, r.workspaceID)
	r.tick(t, 2*time.Hour)
	r.wantReleased(t, "hold removed", ids, 2)
}

// A wakeup-bound row is cancelled as wakeup_superseded when its wakeup is
// disabled, changed or the workspace is paused; an unchanged wakeup releases it.
func TestReleaseControllerWakeupBoundRow(t *testing.T) {
	for _, tc := range []struct {
		name, mutate string
		superseded   bool
	}{
		{"unchanged", `UPDATE issue_wakeup SET updated_at=updated_at WHERE id=$1`, false},
		{"disabled", `UPDATE issue_wakeup SET disabled_at=now(), enabled=false WHERE id=$1`, true},
		{"revision changed", `UPDATE issue_wakeup SET revision=revision+1 WHERE id=$1`, true},
		{"workspace paused", `UPDATE workspace SET scheduling_paused_at=now() WHERE id=(SELECT workspace_id FROM issue_wakeup WHERE id=$1)`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReleaseRig(t)
			r.closedBreaker(t, r.base.Add(-time.Minute))
			wakeup := util.UUIDToString(dbid.NewV7())
			r.x(t, `INSERT INTO issue_wakeup (id, workspace_id, issue_id, agent_id, created_by, instruction, kind, mode, revision)
				VALUES ($1, $2, $3, $4, $4, 'wake', 'at', 'once', 1)`, wakeup, r.workspaceID, r.issue, r.agentID)
			id := r.heldRow(t, r.base.Add(-time.Hour), time.Hour, fmt.Sprintf(`,"wakeup_id":%q,"wakeup_revision":1`, wakeup))
			r.x(t, tc.mutate, wakeup)
			r.tick(t, 0)
			var status, reason string
			var held bool
			if err := r.pool.QueryRow(context.Background(),
				`SELECT status, COALESCE(failure_reason, ''), context ? 'provider_hold' FROM agent_task_queue WHERE id=$1`, id).Scan(&status, &reason, &held); err != nil {
				t.Fatal(err)
			}
			if tc.superseded && (status != "cancelled" || reason != "wakeup_superseded") {
				t.Fatalf("status %q reason %q, want cancelled wakeup_superseded", status, reason)
			}
			if !tc.superseded && (held || status != "deferred") {
				t.Fatalf("status %q held %v, want released and still deferred", status, held)
			}
		})
	}
}

// One account: the only schedulable runtime serves the canary and closes the
// breaker. Open and due -> controller starts the probe -> promote -> claim ->
// success -> closed.
func TestReleaseControllerOneAccountClosesBreaker(t *testing.T) {
	r := newReleaseRig(t)
	ctx := context.Background()
	r.breaker(t, "open", -time.Second, 300, pgtype.UUID{})
	ids := r.heldRows(t, 1)
	r.x(t, `UPDATE agent_runtime SET status='online', last_seen_at=now() WHERE id=$1`, r.runtimeID)
	r.tick(t, 0)
	r.wantReleased(t, "canary", ids, 1)
	if _, err := r.svc.Queries.PromoteDueDeferredTasksForRuntime(ctx, db.PromoteDueDeferredTasksForRuntimeParams{RuntimeID: r.runtimeID, RuntimeStaleSecs: 3600}); err != nil {
		t.Fatal(err)
	}
	claimed, err := r.svc.ClaimTask(ctx, r.agentID)
	if err != nil || claimed == nil || claimed.ID != ids[0] {
		t.Fatalf("ClaimTask = %v, %v; want the released canary", claimed, err)
	}
	r.settle(t, claimed.ID, "completed", "", time.Minute)
	if err := recordProviderSuccess(ctx, db.New(r.pool), db.AgentTaskQueue{ID: claimed.ID, AgentID: r.agentID, Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	if s := r.breakerState(t); s != "closed" {
		t.Fatalf("state %q after the canary succeeded, want closed", s)
	}
}

// D5: a sole held recovery retry is the canary. When it fails, recovery ends:
// no further retry and one owner notice.
func TestReleaseControllerSoleRecoveryRetryFailureEndsRecovery(t *testing.T) {
	o := newOwnerFixture(t)
	ctx := context.Background()
	o.Exec(t, `UPDATE agent SET model='gpt-5.6-sol' WHERE id=$1`, o.agentID)
	recovery := o.recoveryTask(t, testutil.Cols{"max_attempts": 3})
	base := time.Now().UTC().Truncate(time.Second)
	hold := fmt.Sprintf(`{"provider_hold":{"provider":"openai","since":%q,"max_hold_until":"2099-01-01T00:00:00Z"}}`, base.Add(-time.Hour).Format(time.RFC3339))
	o.Exec(t, `UPDATE agent_task_queue SET status='deferred', started_at=NULL, fire_at=now()-interval '1 hour', context=$2::jsonb WHERE id=$1`, recovery, hold)
	o.Exec(t, `INSERT INTO provider_breaker (workspace_id, provider, state, cool_off_s) VALUES ($1, 'openai', 'closed', 300)
		ON CONFLICT (workspace_id, provider) DO UPDATE SET state='closed', canary_task_id=NULL`, o.WorkspaceID)
	o.Cleanup(t, `DELETE FROM provider_breaker WHERE workspace_id=$1`, o.WorkspaceID)

	svc := NewTaskService(o.q, o.Pool, nil, events.New())
	if err := svc.ReleaseProviderHeldTasks(ctx, base); err != nil {
		t.Fatal(err)
	}
	var held bool
	if err := o.Pool.QueryRow(ctx, `SELECT context ? 'provider_hold' FROM agent_task_queue WHERE id=$1`, recovery).Scan(&held); err != nil || held {
		t.Fatalf("sole recovery retry held=%v err=%v, want it released as the canary", held, err)
	}
	o.Exec(t, `UPDATE agent_task_queue SET status='running', started_at=now(), fire_at=NULL WHERE id=$1`, recovery)
	s := o.failWith(t, recovery, held5xx)
	s.wantFailed(t, held5xx)
	if s.children != 0 || s.reservations != 0 || s.notices != 1 {
		t.Fatalf("canary failure children=%d reservations=%d notices=%d, want 0, 0, 1", s.children, s.reservations, s.notices)
	}
	if err := svc.ReleaseProviderHeldTasks(ctx, base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n, notices := o.retryChildren(t, recovery), o.countNotices(t, recovery); n != 0 || notices != 1 {
		t.Fatalf("after the next tick children=%d notices=%d, want 0, 1", n, notices)
	}
}
