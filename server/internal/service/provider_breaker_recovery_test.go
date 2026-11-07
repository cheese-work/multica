package service

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type breakerRig struct {
	pool                        *pgxpool.Pool
	workspaceID, agentID, issue pgtype.UUID
	runtimeID                   pgtype.UUID
}

func newBreakerRig(t *testing.T) *breakerRig {
	t.Helper()
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	workspaceID, _, agentID, issueID := seedAttributionFixture(t, pool)
	r := &breakerRig{
		pool: pool, workspaceID: util.MustParseUUID(workspaceID),
		agentID: util.MustParseUUID(agentID), issue: util.MustParseUUID(issueID),
	}
	if _, err := pool.Exec(ctx, `UPDATE agent SET model='gpt-5.6-sol', max_concurrent_tasks=5 WHERE id=$1`, r.agentID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT runtime_id FROM agent WHERE id=$1`, r.agentID).Scan(&r.runtimeID); err != nil {
		t.Fatal(err)
	}
	return r
}

// task inserts a task row; failedAgo > 0 stamps completed_at that far back.
func (r *breakerRig) task(t *testing.T, status, reason string, ago time.Duration) pgtype.UUID {
	t.Helper()
	issue := r.issue
	if status == "queued" { // one pending task per issue/agent: use a fresh issue
		if err := r.pool.QueryRow(context.Background(), `
			INSERT INTO issue (workspace_id, number, title, creator_type, creator_id, assignee_type, assignee_id, priority)
			SELECT workspace_id, (SELECT COALESCE(max(number), 0) + 1 FROM issue WHERE workspace_id = i.workspace_id), 'breaker issue', creator_type, creator_id, assignee_type, assignee_id, priority FROM issue i WHERE id=$1
			RETURNING id`, r.issue).Scan(&issue); err != nil {
			t.Fatal(err)
		}
	}
	var id pgtype.UUID
	if err := r.pool.QueryRow(context.Background(), `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, attempt, max_attempts, failure_reason, completed_at)
		VALUES ($1, $2, $3, $4, 0, 1, 3, NULLIF($5, ''), CASE WHEN $4 = 'queued' THEN NULL ELSE now() - make_interval(secs => $6::float8) END)
		RETURNING id`, r.agentID, r.runtimeID, issue, status, reason, ago.Seconds()).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (r *breakerRig) breaker(t *testing.T, state string, probeOffset time.Duration, coolOff int, canary pgtype.UUID) {
	t.Helper()
	if _, err := r.pool.Exec(context.Background(), `
		INSERT INTO provider_breaker (workspace_id, provider, state, opened_at, next_probe_at, cool_off_s, canary_task_id)
		VALUES ($1, 'openai', $2, now(), now() + make_interval(secs => $3::float8), $4, $5)
		ON CONFLICT (workspace_id, provider) DO UPDATE SET state=EXCLUDED.state, next_probe_at=EXCLUDED.next_probe_at,
			cool_off_s=EXCLUDED.cool_off_s, canary_task_id=EXCLUDED.canary_task_id`,
		r.workspaceID, state, probeOffset.Seconds(), coolOff, canary); err != nil {
		t.Fatal(err)
	}
}

func (r *breakerRig) row(t *testing.T) (state string, coolOff int, canary pgtype.UUID, exists bool) {
	t.Helper()
	err := r.pool.QueryRow(context.Background(), `SELECT state, cool_off_s, canary_task_id FROM provider_breaker WHERE workspace_id=$1 AND provider='openai'`, r.workspaceID).Scan(&state, &coolOff, &canary)
	return state, coolOff, canary, err == nil
}

func (r *breakerRig) fail(t *testing.T, task pgtype.UUID) {
	t.Helper()
	if err := recordProviderServerFailure(context.Background(), db.New(r.pool), db.AgentTaskQueue{ID: task, AgentID: r.agentID}); err != nil {
		t.Fatal(err)
	}
}

func TestProviderBreakerBurstCountsAsOneSignal(t *testing.T) {
	r := newBreakerRig(t)
	const pse = "agent_error.provider_server_error"
	r.task(t, "failed", pse, 3*time.Second)
	r.task(t, "failed", pse, 2*time.Second)
	last := r.task(t, "failed", pse, time.Second)
	r.fail(t, last)
	if state, _, _, ok := r.row(t); ok {
		t.Fatalf("3 failures inside one 5s burst opened the breaker (state %q); a burst is one signal", state)
	}

	// Distinct signals spread over the window still trip.
	r.task(t, "failed", pse, 2*time.Minute)
	r.task(t, "failed", pse, time.Minute)
	r.fail(t, last)
	if state, _, _, ok := r.row(t); !ok || state != "open" {
		t.Fatalf("3 distinct signals did not open the breaker (state %q, exists %v)", state, ok)
	}
}

func TestProviderBreakerUnknownFailureNeverCounts(t *testing.T) {
	r := newBreakerRig(t)
	var last pgtype.UUID
	for _, ago := range []time.Duration{2 * time.Minute, time.Minute, 10 * time.Second} {
		last = r.task(t, "failed", "agent_error.unknown", ago)
	}
	r.fail(t, last)
	if state, _, _, ok := r.row(t); ok {
		t.Fatalf("agent_error.unknown opened the breaker (state %q)", state)
	}
}

func TestProviderBreakerAdmitProbesOnceDue(t *testing.T) {
	r := newBreakerRig(t)
	q := db.New(r.pool)
	for _, tc := range []struct {
		state  string
		offset time.Duration
		want   string
	}{
		{"open", time.Minute, "hold"},
		{"open", -time.Second, "normal"},
		{"half_open", time.Minute, "hold"},
		{"half_open", -time.Second, "normal"},
	} {
		r.breaker(t, tc.state, tc.offset, 300, pgtype.UUID{})
		got, err := ProviderBreakerAdmit(context.Background(), q, r.workspaceID, "openai")
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("%s with next_probe_at %v: admit = %q, want %q", tc.state, tc.offset, got, tc.want)
		}
	}
}

func TestClaimTaskHalfOpenAdmitsExactlyOneCanary(t *testing.T) {
	r := newBreakerRig(t)
	ctx := context.Background()
	svc := &TaskService{Queries: db.New(r.pool), TxStarter: r.pool, Bus: events.New()}
	first := r.task(t, "queued", "", 0)
	r.task(t, "queued", "", 0)

	r.breaker(t, "open", time.Minute, 300, pgtype.UUID{})
	if got, err := svc.ClaimTask(ctx, r.agentID); err != nil || got != nil {
		t.Fatalf("claim before next_probe_at = %v, %v; want nothing claimed", got, err)
	}

	r.breaker(t, "open", -time.Second, 300, pgtype.UUID{})
	got, err := svc.ClaimTask(ctx, r.agentID)
	if err != nil || got == nil {
		t.Fatalf("due claim = %v, %v; want the canary task", got, err)
	}
	if got.ID != first {
		t.Fatalf("canary = %s, want oldest queued task", util.UUIDToString(got.ID))
	}
	state, _, canary, _ := r.row(t)
	if state != "half_open" || canary != got.ID {
		t.Fatalf("after canary claim: state %q canary %v; want half_open bound to the claimed task", state, canary)
	}
	if again, err := svc.ClaimTask(ctx, r.agentID); err != nil || again != nil {
		t.Fatalf("second claim while half_open = %v, %v; want nothing (exactly one canary)", again, err)
	}
}

func TestProviderBreakerClosesOnCanarySuccess(t *testing.T) {
	r := newBreakerRig(t)
	canary := r.task(t, "completed", "", 0)
	other := r.task(t, "completed", "", 0)
	r.breaker(t, "half_open", time.Hour, 900, canary)

	if err := recordProviderSuccess(context.Background(), db.New(r.pool), db.AgentTaskQueue{ID: other, AgentID: r.agentID, Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	if state, _, _, _ := r.row(t); state != "half_open" {
		t.Fatalf("a non-canary success moved state to %q; only the canary may close", state)
	}
	if err := recordProviderSuccess(context.Background(), db.New(r.pool), db.AgentTaskQueue{ID: canary, AgentID: r.agentID, Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	state, coolOff, c, _ := r.row(t)
	if state != "closed" || c.Valid || coolOff != providerBreakerCoolOff {
		t.Fatalf("after canary success: state %q canary valid %v cool_off %d; want closed, no canary, default cool-off", state, c.Valid, coolOff)
	}
}

func TestProviderBreakerReopensWithLongerCoolOffOnCanaryFailure(t *testing.T) {
	r := newBreakerRig(t)
	canary := r.task(t, "failed", "agent_error.provider_server_error", 0)
	r.breaker(t, "half_open", time.Hour, 300, canary)
	r.fail(t, canary)
	state, coolOff, c, _ := r.row(t)
	if state != "open" || c.Valid || coolOff != 600 {
		t.Fatalf("after canary failure: state %q canary valid %v cool_off %d; want open, no canary, 600", state, c.Valid, coolOff)
	}

	// Repeated failures double up to the cap and never beyond.
	for i := 0; i < 6; i++ {
		r.breaker(t, "half_open", time.Hour, coolOff, canary)
		r.fail(t, canary)
		_, coolOff, _, _ = r.row(t)
	}
	if coolOff != providerBreakerMaxCoolOff {
		t.Fatalf("cool_off after repeated canary failures = %d, want cap %d", coolOff, providerBreakerMaxCoolOff)
	}
}

// A canary that has not settled (any non-terminal status) past its deadline
// keeps the breaker held: no second canary may start, and the canary's own late
// success still closes the breaker.
func TestProviderBreakerActiveCanaryPastDeadlineIsNotReplaced(t *testing.T) {
	for _, status := range []string{"dispatched", "running", "waiting_local_directory"} {
		t.Run(status, func(t *testing.T) {
			r := newBreakerRig(t)
			ctx := context.Background()
			svc := &TaskService{Queries: db.New(r.pool), TxStarter: r.pool, Bus: events.New()}
			canary := r.task(t, status, "", 0)
			r.task(t, "queued", "", 0)
			r.breaker(t, "half_open", -time.Second, 300, canary)

			if got, err := ProviderBreakerAdmit(ctx, db.New(r.pool), r.workspaceID, "openai"); err != nil || got != "hold" {
				t.Fatalf("admit with a %s canary past deadline = %q, %v; want hold", status, got, err)
			}
			if got, err := svc.ClaimTask(ctx, r.agentID); err != nil || got != nil {
				t.Fatalf("claim with a %s canary past deadline = %v, %v; want nothing (no second canary)", status, got, err)
			}
			if _, _, c, _ := r.row(t); c != canary {
				t.Fatalf("canary was replaced: %v, want %v", c, canary)
			}

			if _, err := r.pool.Exec(ctx, `UPDATE agent_task_queue SET status='completed', completed_at=now() WHERE id=$1`, canary); err != nil {
				t.Fatal(err)
			}
			if err := recordProviderSuccess(ctx, db.New(r.pool), db.AgentTaskQueue{ID: canary, AgentID: r.agentID, Status: "completed"}); err != nil {
				t.Fatal(err)
			}
			if state, _, c, _ := r.row(t); state != "closed" || c.Valid {
				t.Fatalf("late canary success: state %q canary valid %v; want closed", state, c.Valid)
			}
		})
	}
}

// A canary that is not in flight (settled without reporting, or back in queued
// where held claims cannot start it) is replaced once its deadline passes.
func TestProviderBreakerIdleCanaryPastDeadlineIsReplaced(t *testing.T) {
	for _, status := range []string{"cancelled", "queued"} {
		t.Run(status, func(t *testing.T) {
			r := newBreakerRig(t)
			ctx := context.Background()
			svc := &TaskService{Queries: db.New(r.pool), TxStarter: r.pool, Bus: events.New()}
			canary := r.task(t, status, "", 0)
			next := r.task(t, "queued", "", 0)
			r.breaker(t, "half_open", -time.Second, 300, canary)

			got, err := svc.ClaimTask(ctx, r.agentID)
			if err != nil || got == nil {
				t.Fatalf("claim after a %s canary's deadline = %v, %v; want a new canary", status, got, err)
			}
			if got.ID != canary && got.ID != next {
				t.Fatalf("claimed unrelated task %s", util.UUIDToString(got.ID))
			}
			if state, _, c, _ := r.row(t); state != "half_open" || c != got.ID {
				t.Fatalf("state %q canary %v; want half_open bound to the claimed task", state, c)
			}
		})
	}
}

// A canary whose claim is rolled back to queued (payload finalization failed)
// must release its binding, or the provider is held with no task able to run.
func TestProviderBreakerRequeuedCanaryIsReelected(t *testing.T) {
	r := newBreakerRig(t)
	ctx := context.Background()
	svc := &TaskService{Queries: db.New(r.pool), TxStarter: r.pool, Bus: events.New()}
	first := r.task(t, "queued", "", 0)
	r.breaker(t, "open", -time.Second, 300, pgtype.UUID{})

	canary, err := svc.ClaimTask(ctx, r.agentID)
	if err != nil || canary == nil || canary.ID != first {
		t.Fatalf("canary claim = %v, %v; want the queued task", canary, err)
	}
	if _, err := svc.RequeueTaskAfterClaimFailure(ctx, *canary); err != nil {
		t.Fatal(err)
	}

	again, err := svc.ClaimTask(ctx, r.agentID)
	if err != nil || again == nil || again.ID != first {
		t.Fatalf("claim after canary requeue = %v, %v; want the same task re-elected as canary", again, err)
	}
	if state, _, c, _ := r.row(t); state != "half_open" || c != first {
		t.Fatalf("state %q canary %v; want half_open bound to the re-claimed task", state, c)
	}
}
