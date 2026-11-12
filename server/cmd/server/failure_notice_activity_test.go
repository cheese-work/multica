package main

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-4575: a terminal gateway-class failure still writes the task_failed
// activity row (actor = the failed agent) and the failure comment, and enqueues
// no further task.
func TestTerminalGatewayFailureKeepsActivityRow(t *testing.T) {
	ctx := context.Background()
	issueID, agentID, runtimeID := setupRerunTestFixture(t)
	t.Cleanup(func() {
		cleanupActivities(t, issueID)
		testPool.Exec(ctx, `DELETE FROM comment WHERE issue_id = $1`, issueID)
		cleanupRerunFixture(t, issueID)
	})

	var taskID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, attempt, max_attempts)
		VALUES ($1, $2, $3, 'running', 0, 3, 3) RETURNING id`, agentID, runtimeID, issueID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	queries := db.New(testPool)
	bus := events.New()
	registerActivityListeners(bus, queries)
	svc := service.NewTaskService(queries, testPool, nil, bus)

	if _, err := svc.FailTask(ctx, parseUUID(taskID), "API Error: 503 service unavailable", "", "", "", "agent_error.provider_server_error", false, "", ""); err != nil {
		t.Fatalf("FailTask: %v", err)
	}

	var failedRows int
	var actorType, actorID string
	if err := testPool.QueryRow(ctx, `
		SELECT count(*), COALESCE(max(actor_type), ''), COALESCE(max(actor_id::text), '')
		FROM activity_log WHERE issue_id = $1 AND action = 'task_failed'`, issueID).Scan(&failedRows, &actorType, &actorID); err != nil {
		t.Fatalf("read activity: %v", err)
	}
	if failedRows != 1 || actorType != "agent" || actorID != agentID {
		t.Fatalf("task_failed activity = (%d, %q, %q), want (1, agent, %s)", failedRows, actorType, actorID, agentID)
	}

	var comments, tasks int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM comment WHERE issue_id = $1 AND type = 'system' AND content = $2`,
		issueID, "API Error: 503 service unavailable").Scan(&comments); err != nil {
		t.Fatalf("count comments: %v", err)
	}
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID).Scan(&tasks); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if comments != 1 || tasks != 1 {
		t.Fatalf("failure comments=%d tasks=%d, want 1 each", comments, tasks)
	}
}
