package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// dispatchedTaskFixture seeds a workspace with one online public runtime, one
// agent and one dispatched, never-started task whose dispatch is 10 min old.
func dispatchedTaskFixture(t *testing.T) (q *db.Queries, queryCount func(kind string) int, workspaceID, runtimeID, taskID pgtype.UUID, dispatchedAt pgtype.Timestamptz) {
	t.Helper()
	pool := sharedTestPool(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	if err := pool.QueryRow(ctx,
		`INSERT INTO workspace (name, slug, description, issue_prefix) VALUES ($1, $2, '', 'EVT') RETURNING id`,
		"Event Test "+suffix, "event-test-"+suffix).Scan(&workspaceID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		pool.Exec(bg, `DELETE FROM agent_task_event WHERE workspace_id = $1`, workspaceID)
		pool.Exec(bg, `DELETE FROM agent_task_queue WHERE agent_id IN (SELECT id FROM agent WHERE workspace_id = $1)`, workspaceID)
		pool.Exec(bg, `DELETE FROM agent WHERE workspace_id = $1`, workspaceID)
		pool.Exec(bg, `DELETE FROM agent_runtime WHERE workspace_id = $1`, workspaceID)
		pool.Exec(bg, `DELETE FROM workspace WHERE id = $1`, workspaceID)
	})
	if err := pool.QueryRow(ctx, `INSERT INTO agent_runtime (workspace_id, name, runtime_mode, provider, status, visibility, last_seen_at)
		VALUES ($1, 'evt', 'local', 'claude', 'online', 'public', now()) RETURNING id`, workspaceID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	var agentID pgtype.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO agent (workspace_id, name, runtime_mode, runtime_id)
		VALUES ($1, 'evt', 'local', $2) RETURNING id`, workspaceID, runtimeID).Scan(&agentID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO agent_task_queue (agent_id, runtime_id, status, dispatched_at)
		VALUES ($1, $2, 'dispatched', now() - interval '10 minutes') RETURNING id, dispatched_at`,
		agentID, runtimeID).Scan(&taskID, &dispatchedAt); err != nil {
		t.Fatal(err)
	}
	queryCount = func(kind string) int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_event WHERE workspace_id = $1 AND task_id = $2 AND runtime_id = $3 AND kind = $4`,
			workspaceID, taskID, runtimeID, kind).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	return db.New(pool), queryCount, workspaceID, runtimeID, taskID, dispatchedAt
}

// A forced claim failure writes exactly one requeue_claim_failure event; a
// stale CAS that updates no row writes none.
func TestRequeueAfterClaimFailureWritesOneEvent(t *testing.T) {
	q, count, _, runtimeID, taskID, dispatchedAt := dispatchedTaskFixture(t)
	ctx := context.Background()
	params := db.RequeueAgentTaskAfterClaimFailureParams{TaskID: taskID, RuntimeID: runtimeID, DispatchedAt: dispatchedAt}
	if _, err := q.RequeueAgentTaskAfterClaimFailure(ctx, params); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if n := count("requeue_claim_failure"); n != 1 {
		t.Fatalf("events after requeue = %d, want 1", n)
	}
	if _, err := q.RequeueAgentTaskAfterClaimFailure(ctx, params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second requeue err = %v, want no rows", err)
	}
	if n := count("requeue_claim_failure"); n != 1 {
		t.Fatalf("events after no-op requeue = %d, want 1", n)
	}
}

// The singular and batch stale reclaim each write one stale_reclaim event per
// re-delivered task, and none when nothing is stale.
func TestStaleReclaimWritesEvent(t *testing.T) {
	for _, mode := range []string{"single", "batch"} {
		t.Run(mode, func(t *testing.T) {
			q, count, _, runtimeID, _, _ := dispatchedTaskFixture(t)
			ctx := context.Background()
			reclaim := func() (int, error) {
				if mode == "single" {
					_, err := q.ReclaimStaleDispatchedTaskForRuntime(ctx, db.ReclaimStaleDispatchedTaskForRuntimeParams{
						RuntimeID: runtimeID, PrepareLeaseSecs: 60, ClaimRecoverySecs: 30, RuntimeStaleSecs: RuntimeClaimFreshnessSeconds})
					if errors.Is(err, pgx.ErrNoRows) {
						return 0, nil
					}
					return 1, err
				}
				rows, err := q.ReclaimStaleDispatchedTasksForRuntimes(ctx, db.ReclaimStaleDispatchedTasksForRuntimesParams{
					RuntimeIds: []pgtype.UUID{runtimeID}, PrepareLeaseSecs: 60, ClaimRecoverySecs: 30,
					RuntimeStaleSecs: RuntimeClaimFreshnessSeconds, MaxTasks: 10})
				return len(rows), err
			}
			if n, err := reclaim(); err != nil || n != 1 {
				t.Fatalf("first reclaim = %d, %v; want 1", n, err)
			}
			if n := count("stale_reclaim"); n != 1 {
				t.Fatalf("events after reclaim = %d, want 1", n)
			}
			time.Sleep(10 * time.Millisecond)
			if n, err := reclaim(); err != nil || n != 0 {
				t.Fatalf("second reclaim = %d, %v; want 0 (dispatch is fresh)", n, err)
			}
			if n := count("stale_reclaim"); n != 1 {
				t.Fatalf("events after no-op reclaim = %d, want 1", n)
			}
		})
	}
}
