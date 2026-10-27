package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// A human message queued while the DM's running task calls continue is
// answered in the new DM: the queued task and its message move with the
// continuation, and the archived DM keeps nothing a daemon can claim.
func TestContinueChatSessionMovesQueuedTurns(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	for _, taskScoped := range []bool{true, false} {
		name := "member continue"
		if taskScoped {
			name = "multica chat continue"
		}
		t.Run(name, func(t *testing.T) {
			agentID := createHandlerTestAgent(t, "continue-queued", []byte("[]"))
			runtimeID := handlerTestRuntimeID(t)
			sourceID := dbfx.ChatSession(t, agentID, testutil.Cols{})
			runningID := dbfx.Task(t, agentID, testutil.Cols{"status": "running", "runtime_id": runtimeID, "chat_session_id": sourceID})
			queuedIDs := make([]string, 2)
			messageIDs := make([]string, 2)
			for i, content := range []string{"first queued", "second queued"} {
				queuedIDs[i] = dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "chat_session_id": sourceID})
				dbfx.Exec(t, `UPDATE agent_task_queue SET chat_input_task_id = id WHERE id = $1`, queuedIDs[i])
				messageIDs[i] = dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": sourceID, "task_id": queuedIDs[i], "role": "user", "content": content})
			}

			path, taskID := "/api/chat/sessions/"+sourceID+"/continue", ""
			if taskScoped {
				path, taskID = "/api/chat/continue", runningID
			}
			request := withChatTestWorkspaceCtx(t, newRequest(http.MethodPost, path, map[string]any{"summary": "Goal: retain the context."}))
			if taskScoped {
				request.Header.Set("X-Actor-Source", "task_token")
				request.Header.Set("X-Task-ID", taskID)
				request.Header.Set("X-Agent-ID", agentID)
			} else {
				request = withURLParam(request, "sessionId", sourceID)
			}
			var result ChatContinueResponse
			testutil.Call(t, testHandler.ContinueChatSession, request).Want(http.StatusCreated).JSON(&result)
			dbfx.Cleanup(t, `DELETE FROM chat_message WHERE chat_session_id = $1`, result.Session.ID)
			dbfx.Cleanup(t, `DELETE FROM chat_session WHERE id = $1`, result.Session.ID)

			for i := range queuedIDs {
				var taskSession, messageSession string
				var afterSummary bool
				if err := testPool.QueryRow(t.Context(), `SELECT chat_session_id::text FROM agent_task_queue WHERE id = $1`, queuedIDs[i]).Scan(&taskSession); err != nil {
					t.Fatal(err)
				}
				if err := testPool.QueryRow(t.Context(), `
					SELECT m.chat_session_id::text, m.created_at > s.created_at
					FROM chat_message m, chat_message s
					WHERE m.id = $1 AND s.chat_session_id = $2 AND s.message_kind = 'continuation_summary'`,
					messageIDs[i], result.Session.ID).Scan(&messageSession, &afterSummary); err != nil {
					t.Fatalf("queued message %d is not in the new DM: %v", i, err)
				}
				if taskSession != result.Session.ID || messageSession != result.Session.ID || !afterSummary {
					t.Errorf("queued turn %d: task in %s, message in %s, after summary %v; want new DM %s", i, taskSession, messageSession, afterSummary, result.Session.ID)
				}
			}
			if count := dbfx.Count(t, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND task_id = ANY($2)`, result.Session.ID, queuedIDs); count != 2 {
				t.Errorf("new DM holds %d queued messages, want 2", count)
			}
			if count := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1 AND status IN ('queued', 'deferred')`, sourceID); count != 0 {
				t.Errorf("archived DM still has %d claimable tasks", count)
			}
			var runningSession string
			if err := testPool.QueryRow(t.Context(), `SELECT chat_session_id::text FROM agent_task_queue WHERE id = $1`, runningID).Scan(&runningSession); err != nil || runningSession != sourceID {
				t.Errorf("running task moved: session %s, err %v", runningSession, err)
			}
		})
	}
}
