package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-4147: Xong, Để sau and Xong tất cả are human-only. A run's task token is refused in every
// MULTICA_TASK_WRITE_SCOPE mode (unset, off, audit, on), and the owner's obligations stay open.
func TestChatAttentionDeniedToTaskTokenInEveryMode(t *testing.T) {
	for _, mode := range []string{"", "off", "audit", "on"} {
		t.Run("mode="+mode, func(t *testing.T) {
			t.Setenv("MULTICA_TASK_WRITE_SCOPE", mode)
			server := httptest.NewServer(NewRouter(testPool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil))
			defer server.Close()

			fixture := workspaceFixture(t)
			runtimeID := fixture.Runtime(t, "Attn deny runtime")
			agent := fixture.Agent(t, "Attn deny agent", runtimeID)
			task := fixture.Task(t, agent, testutil.Cols{"runtime_id": runtimeID, "status": "running"})
			token := mintAgentTaskToken(t, agent, task, testUserID)
			chat := fixture.ChatSession(t, agent, testutil.Cols{"explicitly_created_at": time.Now()})
			item := fixture.Insert(t, "chat_attention_item", testutil.Cols{
				"workspace_id": testWorkspaceID, "chain_root_session_id": chat, "chat_session_id": chat,
				"user_id": testUserID, "kind": "ask", "agent_id": agent,
			})
			fixture.Cleanup(t, `DELETE FROM chat_attention_mark WHERE user_id = $1`, testUserID)
			key := `{"subject_keys":["item:` + item + `"],"request_id":"` + uuid.NewString() + `"}`

			for _, path := range []string{
				"/api/chat/sessions/" + chat + "/attention/xong",
				"/api/chat/sessions/" + chat + "/attention/later",
				"/api/chat/attention/xong-all",
				"/api/chat/attention/xong-all/" + uuid.NewString() + "/undo",
			} {
				request, err := http.NewRequest("POST", server.URL+path, bytes.NewReader([]byte(key)))
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Authorization", "Bearer "+token)
				request.Header.Set("X-Workspace-ID", testWorkspaceID)
				request.Header.Set("Content-Type", "application/json")
				response, err := server.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				if response.StatusCode != http.StatusForbidden {
					t.Errorf("POST %s = %d, want 403", path, response.StatusCode)
				}
			}
			if n := fixture.Count(t, `SELECT count(*) FROM chat_attention_item WHERE id = $1 AND resolved_at IS NULL`, item); n != 1 {
				t.Error("the ask was resolved by a task token")
			}
			if n := fixture.Count(t, `SELECT count(*) FROM chat_attention_item WHERE chat_session_id = $1 AND kind = 'later'`, chat); n != 0 {
				t.Error("a task token created a later item")
			}
		})
	}
}
