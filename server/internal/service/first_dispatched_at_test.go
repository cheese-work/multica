package service

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-8749 S7a: agent_task_queue.first_dispatched_at is set once, at the first
// claim, and survives every later requeue or reclaim. dispatched_at is cleared
// or refreshed by those paths, so it cannot answer "how long did this run wait
// for its first dispatch".

type dispatchTimes struct {
	dispatchedAt, firstDispatchedAt pgtype.Timestamptz
}

func readDispatchTimes(t *testing.T, f *claimDecisionFixture, taskID string) dispatchTimes {
	t.Helper()
	var d dispatchTimes
	if err := f.pool.QueryRow(context.Background(),
		`SELECT dispatched_at, first_dispatched_at FROM agent_task_queue WHERE id = $1`, taskID,
	).Scan(&d.dispatchedAt, &d.firstDispatchedAt); err != nil {
		t.Fatalf("read dispatch times: %v", err)
	}
	return d
}

func TestFirstDispatchedAtSetOnceAtFirstClaimAndKeptByRequeue(t *testing.T) {
	f := newClaimDecisionFixture(t, 5)
	ctx := context.Background()
	taskID := f.insertTask(t, "queued")

	if before := readDispatchTimes(t, f, taskID); before.firstDispatchedAt.Valid {
		t.Fatalf("queued task has first_dispatched_at %v, want NULL", before.firstDispatchedAt.Time)
	}

	claimed, err := f.svc.ClaimTaskForRuntime(ctx, f.runtimeID)
	if err != nil || claimed == nil {
		t.Fatalf("first claim = (%v, %v), want the task", claimed, err)
	}
	first := readDispatchTimes(t, f, taskID)
	if !first.firstDispatchedAt.Valid || !first.dispatchedAt.Valid {
		t.Fatalf("after the first claim: first_dispatched_at valid=%v dispatched_at valid=%v, want both set",
			first.firstDispatchedAt.Valid, first.dispatchedAt.Valid)
	}
	if !first.firstDispatchedAt.Time.Equal(first.dispatchedAt.Time) {
		t.Fatalf("first_dispatched_at %v != dispatched_at %v at the first claim", first.firstDispatchedAt.Time, first.dispatchedAt.Time)
	}

	// The claim response was lost: the requeue clears dispatched_at (agent.sql
	// RequeueAgentTaskAfterClaimFailure) and the next poll dispatches again.
	if _, err := f.svc.Queries.RequeueAgentTaskAfterClaimFailure(ctx, db.RequeueAgentTaskAfterClaimFailureParams{
		TaskID:       claimed.ID,
		RuntimeID:    f.runtimeID,
		DispatchedAt: first.dispatchedAt,
	}); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	requeued := readDispatchTimes(t, f, taskID)
	if requeued.dispatchedAt.Valid {
		t.Fatalf("requeued task still has dispatched_at %v", requeued.dispatchedAt.Time)
	}
	if !requeued.firstDispatchedAt.Valid || !requeued.firstDispatchedAt.Time.Equal(first.firstDispatchedAt.Time) {
		t.Fatalf("requeue changed first_dispatched_at to %v, want %v", requeued.firstDispatchedAt, first.firstDispatchedAt.Time)
	}

	time.Sleep(20 * time.Millisecond)
	if again, err := f.svc.ClaimTaskForRuntime(ctx, f.runtimeID); err != nil || again == nil {
		t.Fatalf("second claim = (%v, %v), want the task", again, err)
	}
	second := readDispatchTimes(t, f, taskID)
	if !second.dispatchedAt.Time.After(first.dispatchedAt.Time) {
		t.Fatalf("second dispatched_at %v is not after the first %v", second.dispatchedAt.Time, first.dispatchedAt.Time)
	}
	if !second.firstDispatchedAt.Time.Equal(first.firstDispatchedAt.Time) {
		t.Fatalf("reclaimed run moved first_dispatched_at from %v to %v", first.firstDispatchedAt.Time, second.firstDispatchedAt.Time)
	}
}

func TestFirstDispatchedAtKeptWhenStaleDispatchIsReclaimed(t *testing.T) {
	f := newClaimDecisionFixture(t, 5)
	ctx := context.Background()
	taskID := f.insertTask(t, "dispatched") // dispatched_at 10 minutes ago, lease expired
	if _, err := f.pool.Exec(ctx, `UPDATE agent_task_queue SET first_dispatched_at = dispatched_at WHERE id = $1`, taskID); err != nil {
		t.Fatalf("seed first_dispatched_at: %v", err)
	}
	before := readDispatchTimes(t, f, taskID)

	reclaimed, err := f.svc.ClaimTaskForRuntime(ctx, f.runtimeID)
	if err != nil || reclaimed == nil || util.UUIDToString(reclaimed.ID) != taskID {
		t.Fatalf("reclaim = (%v, %v), want task %s", reclaimed, err, taskID)
	}
	after := readDispatchTimes(t, f, taskID)
	if !after.dispatchedAt.Time.After(before.dispatchedAt.Time) {
		t.Fatalf("reclaim did not refresh dispatched_at (%v -> %v)", before.dispatchedAt.Time, after.dispatchedAt.Time)
	}
	if !after.firstDispatchedAt.Time.Equal(before.firstDispatchedAt.Time) {
		t.Fatalf("reclaim moved first_dispatched_at from %v to %v", before.firstDispatchedAt.Time, after.firstDispatchedAt.Time)
	}
}
