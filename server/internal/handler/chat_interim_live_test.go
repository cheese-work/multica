package handler

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-4704: a text line the agent wrote before a tool call reaches the DM
// while the turn still runs, so a turn that later fails or is cancelled keeps
// it. The line still being typed is not a message yet.
func TestReportTaskMessagesShowsSealedChatTextWhileTurnRuns(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	runtimeID := handlerTestRuntimeID(t)
	agentID := dbfx.Agent(t, "interim live agent", runtimeID, testutil.Cols{
		"instructions": "",
		"custom_env":   testutil.Raw("'{}'::jsonb"),
		"custom_args":  testutil.Raw("'[]'::jsonb"),
	})
	chatSessionID := dbfx.ChatSession(t, agentID)
	taskID := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id":      runtimeID,
		"chat_session_id": chatSessionID,
		"status":          "running",
		"started_at":      testutil.Raw("now()"),
	})
	if _, err := dbfx.Pool.Exec(ctx, `UPDATE agent_task_queue SET chat_input_task_id = id WHERE id = $1`, taskID); err != nil {
		t.Fatal(err)
	}

	testutil.Call(t, testHandler.ReportTaskMessages, batchMessagesRequest(t, taskID, []any{
		map[string]any{"seq": 1, "type": "text", "content": "Creating "},
		map[string]any{"seq": 2, "type": "text", "content": "the issue…"},
		map[string]any{"seq": 3, "type": "tool_use", "tool": "Bash"},
		map[string]any{"seq": 4, "type": "text", "content": "Still typing"},
	})).Want(http.StatusOK)

	rows, err := dbfx.Pool.Query(ctx, `SELECT content FROM chat_message WHERE chat_session_id = $1 AND role = 'assistant' ORDER BY created_at, id`, chatSessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := []string{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		got = append(got, c)
	}
	if want := []string{"Creating the issue…"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("assistant rows = %q, want %q", got, want)
	}
}
