package main

import (
	"context"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The 5 s quick_actions claim deadline must not wait for the 30 s runtime
// sweep (CHE-4103 review round 1): the dedicated loop, on its real ticker,
// has to enforce it within a couple of seconds.
func TestBackgroundDeadlineSweeperEnforcesAShortDeadlineOnItsOwnTicker(t *testing.T) {
	if testPool == nil {
		t.Skip("no database available")
	}
	ctx := context.Background()
	var agentID, runtimeID string
	if err := testPool.QueryRow(ctx, `
		SELECT a.id, a.runtime_id FROM agent a
		JOIN member m ON m.workspace_id = a.workspace_id
		JOIN "user" u ON u.id = m.user_id
		WHERE u.email = $1 LIMIT 1`, integrationTestEmail).Scan(&agentID, &runtimeID); err != nil {
		t.Fatal(err)
	}
	var taskID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, status, priority, purpose, context, created_at)
		VALUES ($1, $2, 'queued', 0, 'bg:quick_actions',
		        '{"type":"background","job":"quick_actions","slot":"primary","claim_deadline_seconds":5,"run_deadline_seconds":20}'::jsonb,
		        now() - interval '6 seconds')
		RETURNING id`, agentID, runtimeID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID) })

	svc := service.NewTaskService(db.New(testPool), testPool, nil, events.New())
	noAgent := make(chan struct{}, 1)
	svc.BackgroundNoAgent = map[string]service.BackgroundNoAgentPath{
		"bg:quick_actions": func(context.Context, db.AgentTaskQueue) { noAgent <- struct{}{} },
	}
	loopCtx, stop := context.WithCancel(ctx)
	defer stop()
	go runBackgroundDeadlineSweeper(loopCtx, svc)

	select {
	case <-noAgent:
	case <-time.After(3 * time.Second):
		t.Fatal("an unclaimed 5 s background task was not handled within 3 s: deadline enforcement is not on its own short ticker")
	}
	var status string
	if err := testPool.QueryRow(ctx, `SELECT status FROM agent_task_queue WHERE id = $1`, taskID).Scan(&status); err != nil || status != "cancelled" {
		t.Fatalf("status = %q (%v), want cancelled", status, err)
	}
}
