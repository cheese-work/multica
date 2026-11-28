package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// A task token carries the runtime owner's user id, not the member who started
// the run. The member-facing session wakeup routes accept the wakeup's
// requester, so for a run another member started on the owner's runtime they
// would act as the owner and hand back the owner's private instruction. Those
// four routes are closed to a task token at the chat route gate; an agent
// manages its chat wakeups on /api/chat/wakeups/*, which names the chat from
// the token's own task.
//
// Everything runs through the real router: Auth stamps the actor from the
// stored token, the gate classes the route, and the real handler answers.
func TestSessionWakeupRoutesAreClosedToATaskToken(t *testing.T) {
	fx := testutil.New(testPool, testWorkspaceID, testUserID)

	var agentID, runtimeID string
	fx.QueryRow(t, `SELECT id, runtime_id FROM agent WHERE workspace_id = $1 AND runtime_id IS NOT NULL LIMIT 1`, testWorkspaceID).
		Scan(&agentID, &runtimeID)

	// The owner's chat, and a second member who starts a run on the owner's runtime.
	chatID := fx.ChatSession(t, agentID, testutil.Cols{"title": "owner chat", "creator_id": testUserID})
	email := fmt.Sprintf("wakeup-originator-%d@multica.test", time.Now().UnixNano())
	member := fx.User(t, "Wakeup Originator", email)
	fx.Member(t, testWorkspaceID, member, "member")

	// B's run: not a turn of the owner's chat. The token is bound to the owner.
	runID := fx.Insert(t, "agent_task_queue", testutil.Cols{
		"agent_id": agentID, "runtime_id": runtimeID, "status": "running", "priority": 0,
		"originator_user_id": member, "accountable_user_id": member,
		"originator_source": "direct_human",
	})
	runToken := mintAgentTaskToken(t, agentID, runID, testUserID)

	// A turn of the owner's chat, for the agent's own-turn route.
	turnID := fx.Insert(t, "agent_task_queue", testutil.Cols{
		"agent_id": agentID, "runtime_id": runtimeID, "status": "running", "priority": 0,
		"chat_session_id":    chatID,
		"originator_user_id": testUserID, "accountable_user_id": testUserID,
		"originator_source": "direct_human",
	})
	turnToken := mintAgentTaskToken(t, agentID, turnID, testUserID)

	const secret = "private-instruction-for-the-owner-only"
	failedTask := fx.Insert(t, "agent_task_queue", testutil.Cols{
		"agent_id": agentID, "runtime_id": runtimeID, "status": "failed", "priority": 0,
		"chat_session_id": chatID,
	})
	// seed stores a wakeup the owner asked for. A retryable one has ended and its
	// report task failed.
	seed := func(t *testing.T, retryable bool) string {
		t.Helper()
		status, last := "active", any(nil)
		if retryable {
			status, last = "expired", failedTask
		}
		var id string
		if err := testPool.QueryRow(ctx(), `
			INSERT INTO chat_wakeup (id, workspace_id, chat_session_id, agent_id, requester_id, title, instruction, kind, status, last_task_id, fire_at, next_eval_at, expires_at)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, 'Release shipped', $5, 'at', $6, $7, now() + interval '1 hour', now() + interval '1 hour', now() + interval '1 hour')
			RETURNING id::text`, testWorkspaceID, chatID, agentID, testUserID, secret, status, last).Scan(&id); err != nil {
			t.Fatalf("seed chat wakeup: %v", err)
		}
		t.Cleanup(func() { testPool.Exec(ctx(), `DELETE FROM chat_wakeup WHERE id = $1`, id) })
		return id
	}
	snapshot := func(t *testing.T, id string) string {
		t.Helper()
		var row string
		if err := testPool.QueryRow(ctx(), `SELECT row_to_json(w)::text FROM chat_wakeup w WHERE id = $1`, id).Scan(&row); err != nil {
			t.Fatalf("read wakeup %s: %v", id, err)
		}
		return row
	}

	do := func(t *testing.T, method, path string, body any, bearer string) (int, string) {
		t.Helper()
		var reader io.Reader = bytes.NewReader(nil)
		if body != nil {
			b, _ := json.Marshal(body)
			reader = bytes.NewReader(b)
		}
		req, err := http.NewRequest(method, testServer.URL+path, reader)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("X-Workspace-ID", testWorkspaceID)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out)
	}

	retime := map[string]any{"at": time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)}
	base := "/api/chat/sessions/" + chatID + "/wakeups/"
	routes := []struct {
		name, method, suffix string
		body                 any
		retryable            bool
	}{
		{"DELETE", http.MethodDelete, "", nil, false},
		{"PATCH", http.MethodPatch, "", retime, false},
		{"cancel", http.MethodPost, "/cancel", nil, false},
		{"retry", http.MethodPost, "/retry", nil, true},
	}

	for _, rt := range routes {
		t.Run("task token on "+rt.name+" is refused", func(t *testing.T) {
			id := seed(t, rt.retryable)
			before := snapshot(t, id)
			code, body := do(t, rt.method, base+id+rt.suffix, rt.body, runToken)
			if code != http.StatusForbidden {
				t.Fatalf("%s = %d %q, want 403", rt.name, code, body)
			}
			if strings.Contains(body, secret) {
				t.Fatalf("%s body leaks the wakeup instruction: %q", rt.name, body)
			}
			if after := snapshot(t, id); after != before {
				t.Fatalf("%s changed the wakeup:\nbefore %s\nafter  %s", rt.name, before, after)
			}
		})
	}

	// The owner's own session, as a person: unchanged.
	for _, rt := range routes {
		t.Run("owner JWT on "+rt.name+" still works", func(t *testing.T) {
			id := seed(t, rt.retryable)
			// retry fires a fresh report task, so the row stays "expired" and its last task moves.
			wantStatus := map[string]string{"DELETE": "cancelled", "PATCH": "active", "cancel": "cancelled", "retry": "expired"}[rt.name]
			code, body := do(t, rt.method, base+id+rt.suffix, rt.body, testToken)
			if code != http.StatusOK {
				t.Fatalf("%s = %d %q, want 200", rt.name, code, body)
			}
			var status string
			if err := testPool.QueryRow(ctx(), `SELECT status FROM chat_wakeup WHERE id = $1`, id).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status != wantStatus {
				t.Fatalf("%s left status %q, want %q", rt.name, status, wantStatus)
			}
			if rt.retryable {
				var last string
				if err := testPool.QueryRow(ctx(), `SELECT coalesce(last_task_id::text, '') FROM chat_wakeup WHERE id = $1`, id).Scan(&last); err != nil {
					t.Fatal(err)
				}
				if last == "" || last == failedTask {
					t.Fatalf("retry did not queue a new report task (last task %q)", last)
				}
			}
		})
	}

	// The agent's own-turn route names the chat from the token's task.
	t.Run("task token on its own-turn cancel still works", func(t *testing.T) {
		id := seed(t, false)
		code, body := do(t, http.MethodPost, "/api/chat/wakeups/"+id+"/cancel", nil, turnToken)
		if code != http.StatusOK {
			t.Fatalf("own-turn cancel = %d %q, want 200", code, body)
		}
		var status string
		if err := testPool.QueryRow(ctx(), `SELECT status FROM chat_wakeup WHERE id = $1`, id).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "cancelled" {
			t.Fatalf("own-turn cancel left status %q, want cancelled", status)
		}
	})
}
