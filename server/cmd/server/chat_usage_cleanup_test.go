package main

import (
	"context"
	"testing"
	"time"
)

// leakedActiveChatlessTasks lists dispatched/running tasks created since
// `since` that belong to no issue and no chat session. Those rows make the
// owning agent `working` for the rest of the package run (see
// RefreshAgentStatusFromTasks), so a test that strands one corrupts every later
// status assertion on the shared integration agent.
func leakedActiveChatlessTasks(t *testing.T, since time.Time) []string {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `
		SELECT q.id::text || ':' || q.status
		FROM agent_task_queue q JOIN agent a ON a.id = q.agent_id
		WHERE a.workspace_id = $1
		  AND q.status IN ('dispatched', 'running')
		  AND q.issue_id IS NULL AND q.chat_session_id IS NULL
		  AND q.created_at >= $2
	`, testWorkspaceID, since)
	if err != nil {
		t.Fatalf("list leaked tasks: %v", err)
	}
	defer rows.Close()
	var leaked []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatalf("scan leaked task: %v", err)
		}
		leaked = append(leaked, row)
	}
	return leaked
}

// A chat-usage test that moves its session to another workspace must still
// remove the session's tasks. chat_session deletion nulls
// agent_task_queue.chat_session_id (ON DELETE SET NULL), so deleting the
// session first strands the seeded running/dispatched tasks on the shared
// integration agent and flips later sweeper tests from idle to working.
func TestChatSessionUsageCrossWorkspaceCleanupLeavesNoActiveTasks(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	start := time.Now().Add(-time.Second)

	t.Run("body", TestChatSessionUsageRefusesCrossWorkspaceLikeSessionGet)

	if leaked := leakedActiveChatlessTasks(t, start); len(leaked) != 0 {
		t.Fatalf("cleanup stranded active tasks with no chat session or issue: %v", leaked)
	}
}
