package main

import (
	"context"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The sweeper's reset of a stuck in_progress issue is a system move: a block
// entry gate on the todo step never holds it (CHE-2531 fix round).
func TestSweepResetPassesGatedTodoStep(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	ctx := context.Background()

	var agentID, runtimeID string
	if err := testPool.QueryRow(ctx, `
		SELECT a.id, a.runtime_id FROM agent a
		JOIN member m ON m.workspace_id = a.workspace_id
		JOIN "user" u ON u.id = m.user_id
		WHERE u.email = $1
		LIMIT 1
	`, integrationTestEmail).Scan(&agentID, &runtimeID); err != nil {
		t.Fatalf("failed to find test agent: %v", err)
	}

	var workflowID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue_workflow (workspace_id, name, initial_status_key, steps)
		VALUES ($1, 'sweeper gated todo', 'todo', $2::jsonb) RETURNING id::text`,
		testWorkspaceID, `[
			{"status_key":"todo","handler":{"type":"none"},"entry":[{"guard":"pr.ready","mode":"block","member_override":true}]},
			{"status_key":"in_progress","handler":{"type":"none"}}
		]`).Scan(&workflowID); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	var issueID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, title, status, priority, creator_type, creator_id, assignee_type, assignee_id, workflow_id)
		SELECT $1, 'Stuck in_progress gated issue', 'in_progress', 'none', 'member', m.user_id, 'agent', $2, $3
		FROM member m WHERE m.workspace_id = $1 LIMIT 1
		RETURNING id`, testWorkspaceID, agentID, workflowID).Scan(&issueID); err != nil {
		t.Fatalf("failed to create test issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
		testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, issueID)
		testPool.Exec(ctx, `DELETE FROM issue_workflow WHERE id = $1`, workflowID)
	})
	if _, err := testPool.Exec(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, max_attempts, dispatched_at, started_at)
		VALUES ($1, $2, $3, 'running', 0, 1, now() - interval '3 hours', now() - interval '3 hours')`,
		agentID, runtimeID, issueID); err != nil {
		t.Fatalf("failed to create stale task: %v", err)
	}

	queries := db.New(testPool)
	ageOutAgentRuntime(t, agentID, defaultRuntimeReconnectGrace+time.Hour)
	failedTasks, err := queries.FailStaleTasks(ctx, db.FailStaleTasksParams{
		DispatchTimeoutSecs:       300.0,
		RunningTimeoutSecs:        1.0,
		RuntimeStaleSecs:          staleThresholdSeconds,
		RuntimeReconnectGraceSecs: defaultRuntimeReconnectGrace.Seconds(),
	})
	if err != nil {
		t.Fatalf("FailStaleTasks failed: %v", err)
	}

	bus := events.New()
	taskSvc := service.NewTaskService(queries, testPool, nil, bus)
	broadcastFailedTasks(ctx, queries, taskSvc, bus, failedTasks)

	var status string
	if err := testPool.QueryRow(ctx, `SELECT status FROM issue WHERE id = $1`, issueID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "todo" {
		t.Fatalf("status = %q, want todo: the sweeper reset must pass a gated todo step", status)
	}
}
