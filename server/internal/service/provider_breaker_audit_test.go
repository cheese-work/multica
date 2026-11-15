package service

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// auditWant returns an assertion that exactly one audit row of event exists with the given states.
func (r *breakerRig) auditWant(t *testing.T) func(event, wantPrevious, wantNext string) {
	ctx := context.Background()
	t.Cleanup(func() {
		r.pool.Exec(ctx, `DELETE FROM provider_breaker_audit WHERE workspace_id = $1`, r.workspaceID)
	})
	return func(event, wantPrevious, wantNext string) {
		t.Helper()
		var n int
		var previous, next string
		if err := r.pool.QueryRow(ctx, `SELECT count(*), COALESCE(max(previous_state), ''), COALESCE(max(new_state), '') FROM provider_breaker_audit WHERE workspace_id = $1 AND event = $2`,
			r.workspaceID, event).Scan(&n, &previous, &next); err != nil {
			t.Fatal(err)
		}
		if n != 1 || previous != wantPrevious || next != wantNext {
			t.Fatalf("audit %q: %d rows %s->%s, want 1 row %s->%s", event, n, previous, next, wantPrevious, wantNext)
		}
	}
}

// CHE-4381: every automatic transition leaves one audit row with its previous and new state.
func TestProviderBreakerOpenIsAudited(t *testing.T) {
	r := newBreakerRig(t)
	want := r.auditWant(t)
	const pse = "agent_error.provider_server_error"
	r.task(t, "failed", pse, 2*time.Minute)
	r.task(t, "failed", pse, time.Minute)
	r.fail(t, r.task(t, "failed", pse, 10*time.Second))
	want("open", "closed", "open")
}

func TestProviderBreakerHalfOpenCloseAndReopenAreAudited(t *testing.T) {
	r := newBreakerRig(t)
	ctx := context.Background()
	want := r.auditWant(t)
	svc := &TaskService{Queries: db.New(r.pool), TxStarter: r.pool, Bus: events.New()}
	r.task(t, "queued", "", 0)
	r.breaker(t, "open", -time.Second, 300, pgtype.UUID{})
	canary, err := svc.ClaimTask(ctx, r.agentID)
	if err != nil || canary == nil {
		t.Fatalf("canary claim = %v, %v", canary, err)
	}
	want("half_open", "open", "half_open")

	if _, err := r.pool.Exec(ctx, `UPDATE agent_task_queue SET status='completed', completed_at=now() WHERE id=$1`, canary.ID); err != nil {
		t.Fatal(err)
	}
	if err := recordProviderSuccess(ctx, db.New(r.pool), db.AgentTaskQueue{ID: canary.ID, AgentID: r.agentID, Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	want("close", "half_open", "closed")

	r.breaker(t, "half_open", time.Hour, 300, canary.ID)
	r.fail(t, canary.ID)
	want("reopen", "half_open", "open")
}
