package service

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// TestWriteChatCompletionOutcomeKeepsInterimTextVisible pins CHE-4704: a text
// block the agent wrote BEFORE its last tool step is a message for the human,
// not step-block noise. Only the final Output used to become a chat_message, so
// the earlier lines stayed in task_message and folded into the step block.
func TestWriteChatCompletionOutcomeKeepsInterimTextVisible(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, userID, agentID, _ := seedAttributionFixture(t, pool)

	var chatSessionID, taskID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO chat_session (workspace_id, agent_id, creator_id)
		VALUES ($1, $2, $3) RETURNING id`, workspaceID, agentID, userID).Scan(&chatSessionID); err != nil {
		t.Fatalf("seed chat session: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, chat_session_id, status, priority)
		VALUES ($1, (SELECT runtime_id FROM agent WHERE id = $1), $2, 'running', 2) RETURNING id::text`,
		agentID, chatSessionID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM task_message WHERE task_id = $1`, taskID)
		pool.Exec(context.Background(), `DELETE FROM chat_message WHERE chat_session_id = $1`, chatSessionID)
		pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID)
		pool.Exec(context.Background(), `DELETE FROM chat_session WHERE id = $1`, chatSessionID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO chat_message (chat_session_id, role, content, task_id)
		VALUES ($1, 'user', 'do the thing', $2)`, chatSessionID, taskID); err != nil {
		t.Fatalf("seed input: %v", err)
	}

	// 2 interim lines (each followed by a tool step), thinking, then the final text.
	for i, m := range []struct{ typ, content string }{
		{"thinking", "hmm"},
		{"text", "Creating the issue…"},
		{"tool_use", ""},
		{"tool_result", ""},
		{"text", "Reading the log…"},
		{"tool_use", ""},
		{"tool_result", ""},
		{"text", "All done."},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO task_message (task_id, seq, type, content) VALUES ($1, $2, $3, NULLIF($4, ''))`,
			taskID, i+1, m.typ, m.content); err != nil {
			t.Fatalf("seed task_message %d: %v", i+1, err)
		}
	}

	svc := &TaskService{Queries: q, TxStarter: pool}
	result, _ := json.Marshal(protocol.TaskCompletedPayload{Output: "All done."})
	if _, err := svc.writeChatCompletionOutcome(ctx, q, db.AgentTaskQueue{
		ID:              util.MustParseUUID(taskID),
		ChatSessionID:   util.MustParseUUID(chatSessionID),
		ChatInputTaskID: util.MustParseUUID(taskID),
		AgentID:         util.MustParseUUID(agentID),
	}, result); err != nil {
		t.Fatalf("writeChatCompletionOutcome: %v", err)
	}

	rows, err := pool.Query(ctx, `
		SELECT content FROM chat_message
		WHERE chat_session_id = $1 AND role = 'assistant' ORDER BY created_at, id`, chatSessionID)
	if err != nil {
		t.Fatalf("list assistant rows: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, c)
	}
	want := []string{"Creating the issue…", "Reading the log…", "All done."}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("assistant rows = %q, want %q", got, want)
	}
}
