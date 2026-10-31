package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestCreateChatWakeupRejectsOverflowingDelay(t *testing.T) {
	for name, afterSeconds := range map[string]int64{
		"just over 24h":    86401,
		"overflows 64-bit": 18446744075,
		"max int64":        1<<63 - 1,
	} {
		req := newRequest(http.MethodPost, "/api/chat/wakeups", map[string]any{"after_seconds": afterSeconds, "instruction": "refresh"})
		req.Header.Set("X-Actor-Source", "task_token")
		req.Header.Set("X-Task-ID", uuid.NewString())
		w := httptest.NewRecorder()
		testHandler.CreateChatWakeup(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", name, w.Code, w.Body.String())
		}
	}
}

func memberWakeupRequest(t *testing.T, method, sessionID, wakeupID string) *http.Request {
	t.Helper()
	path := "/api/chat/sessions/" + sessionID + "/wakeups"
	if wakeupID != "" {
		path += "/" + wakeupID
	}
	req := withURLParams(newRequest(method, path, nil), "sessionId", sessionID, "wakeupId", wakeupID)
	return withChatTestWorkspaceCtx(t, req)
}

func insertPendingWakeup(t *testing.T, sessionID, agentID, instruction string, fireAt time.Time) string {
	t.Helper()
	return dbfx.Insert(t, "chat_wakeup", testutil.Cols{
		"workspace_id": testWorkspaceID, "chat_session_id": sessionID, "agent_id": agentID,
		"instruction": instruction, "fire_at": fireAt, "status": "pending",
	})
}

func TestMemberListsAndCancelsPendingChatWakeups(t *testing.T) {
	agentID := createHandlerTestAgent(t, "member-wakeup-agent", []byte("[]"))
	sessionID := createHandlerTestChatSession(t, agentID)
	later := insertPendingWakeup(t, sessionID, agentID, "second", time.Now().Add(2*time.Hour))
	first := insertPendingWakeup(t, sessionID, agentID, "first", time.Now().Add(time.Hour))
	fired := insertPendingWakeup(t, sessionID, agentID, "already fired", time.Now().Add(time.Hour))
	dbfx.Exec(t, `UPDATE chat_wakeup SET status = 'fired' WHERE id = $1`, fired)

	var listed []ChatWakeupResponse
	testutil.Call(t, testHandler.ListChatSessionWakeups, memberWakeupRequest(t, http.MethodGet, sessionID, "")).Want(http.StatusOK).JSON(&listed)
	if len(listed) != 2 || listed[0].ID != first || listed[1].ID != later {
		t.Fatalf("want pending wakeups [first, later] in fire order, got %+v", listed)
	}
	if listed[0].AgentID != agentID {
		t.Errorf("agent_id = %q, want %q", listed[0].AgentID, agentID)
	}

	testutil.Call(t, testHandler.CancelChatSessionWakeup, memberWakeupRequest(t, http.MethodDelete, sessionID, first)).Want(http.StatusNoContent)
	testutil.Call(t, testHandler.CancelChatSessionWakeup, memberWakeupRequest(t, http.MethodDelete, sessionID, first)).Want(http.StatusNotFound)

	listed = nil
	testutil.Call(t, testHandler.ListChatSessionWakeups, memberWakeupRequest(t, http.MethodGet, sessionID, "")).Want(http.StatusOK).JSON(&listed)
	if len(listed) != 1 || listed[0].ID != later {
		t.Fatalf("want only [later] after cancel, got %+v", listed)
	}
}

func TestMemberCannotManageWakeupsOfSomeoneElsesDM(t *testing.T) {
	agentID := createHandlerTestAgent(t, "member-wakeup-other", []byte("[]"))
	sessionID := createHandlerTestChatSession(t, agentID)
	dbfx.Exec(t, `UPDATE chat_session SET creator_id = $2 WHERE id = $1`, sessionID, createSecondWorkspaceMember(t))
	wakeup := insertPendingWakeup(t, sessionID, agentID, "private", time.Now().Add(time.Hour))

	testutil.Call(t, testHandler.ListChatSessionWakeups, memberWakeupRequest(t, http.MethodGet, sessionID, "")).Want(http.StatusForbidden)
	testutil.Call(t, testHandler.CancelChatSessionWakeup, memberWakeupRequest(t, http.MethodDelete, sessionID, wakeup)).Want(http.StatusForbidden)
}

func sameInstant(got any, wantRFC3339 string) bool {
	text, _ := got.(string)
	gotTime, err := time.Parse(time.RFC3339, text)
	wantTime, wantErr := time.Parse(time.RFC3339, wantRFC3339)
	return err == nil && wantErr == nil && gotTime.Equal(wantTime)
}

func TestCreateChatWakeupRejectsOverlongTitle(t *testing.T) {
	req := newRequest(http.MethodPost, "/api/chat/wakeups", map[string]any{"after_seconds": 60, "instruction": "refresh", "title": strings.Repeat("t", 121)})
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Task-ID", uuid.NewString())
	w := httptest.NewRecorder()
	testHandler.CreateChatWakeup(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "title") {
		t.Fatalf("status = %d body = %s, want 400 naming the title", w.Code, w.Body.String())
	}
}

func TestChatWakeupResponseIncludesTitle(t *testing.T) {
	out, err := json.Marshal(chatWakeupToResponse(db.ChatWakeup{Title: "Check deploy", Instruction: "look"}))
	if err != nil || !strings.Contains(string(out), `"title":"Check deploy"`) {
		t.Fatalf("response = %s, %v; want the title", out, err)
	}
}

// A user message sent by a fired wakeup carries scheduled_wakeup on every
// endpoint that lists messages; other messages, and a task id fired by another
// DM's wakeup, do not. content and message_kind stay the model-facing values.
func TestChatMessagesCarryScheduledWakeup(t *testing.T) {
	ctx := context.Background()
	agentID := createHandlerTestAgent(t, "ChatWakeupMessageAgent", []byte("[]"))
	sessionID := createHandlerTestChatSession(t, agentID)
	otherSessionID := createHandlerTestChatSession(t, agentID)
	firedTask, foreignTask := uuid.NewString(), uuid.NewString()
	const header = "[Scheduled wakeup — you set this earlier in this DM; no person wrote this message.]\nInstruction: look"

	var wakeupID, firedAt, createdAt string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO chat_wakeup (workspace_id, chat_session_id, agent_id, title, instruction, fire_at, status, fired_task_id)
		VALUES ($1, $2, $3, 'Check deploy', 'look', '2026-01-01T00:00:10Z', 'fired', $4)
		RETURNING id, to_char(fire_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), created_at::text`,
		testWorkspaceID, sessionID, agentID, firedTask).Scan(&wakeupID, &firedAt, &createdAt); err != nil {
		t.Fatalf("seed wakeup: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO chat_wakeup (workspace_id, chat_session_id, agent_id, title, instruction, fire_at, status, fired_task_id)
		VALUES ($1, $2, $3, 'Other DM', 'other', now(), 'fired', $4)`,
		testWorkspaceID, otherSessionID, agentID, foreignTask); err != nil {
		t.Fatalf("seed other wakeup: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM chat_wakeup WHERE chat_session_id = ANY($1)`, []string{sessionID, otherSessionID})
	})
	var wakeupMessageID string
	for i, m := range []struct{ content, task string }{
		{"hello", ""}, {header, firedTask}, {"reused task id", foreignTask},
	} {
		var id string
		var task any
		if m.task != "" {
			task = m.task
		}
		if err := testPool.QueryRow(ctx, `
			INSERT INTO chat_message (chat_session_id, role, content, task_id, created_at)
			VALUES ($1, 'user', $2, $3, timestamp '2026-01-01 00:00:00' + ($4::int * interval '1 second'))
			RETURNING id`, sessionID, m.content, task, i).Scan(&id); err != nil {
			t.Fatalf("insert message %d: %v", i, err)
		}
		if m.task == firedTask {
			wakeupMessageID = id
		}
	}

	check := func(endpoint string, rows []map[string]any) {
		t.Helper()
		if len(rows) != 3 {
			t.Fatalf("%s: %d messages, want 3", endpoint, len(rows))
		}
		for _, row := range rows {
			sw, has := row["scheduled_wakeup"]
			if row["id"] != wakeupMessageID {
				if has {
					t.Errorf("%s: message %q has scheduled_wakeup %v, want none", endpoint, row["content"], sw)
				}
				continue
			}
			got, _ := sw.(map[string]any)
			if got["id"] != wakeupID || got["agent_id"] != agentID || got["title"] != "Check deploy" ||
				got["instruction"] != "look" || !sameInstant(got["fire_at"], firedAt) || got["created_at"] == nil || got["created_at"] == "" {
				t.Errorf("%s: scheduled_wakeup = %v, want id %s agent %s title/instruction/fire_at %s", endpoint, sw, wakeupID, agentID, firedAt)
			}
			if row["content"] != header || row["message_kind"] != "message" || row["role"] != "user" {
				t.Errorf("%s: content/kind/role = %q/%v/%v, want unchanged", endpoint, row["content"], row["message_kind"], row["role"])
			}
		}
	}

	list := httptest.NewRequest(http.MethodGet, "/api/chat/sessions/"+sessionID+"/messages", nil)
	list.Header.Set("X-User-ID", testUserID)
	list = withChatTestWorkspaceCtx(t, withURLParam(list, "sessionId", sessionID))
	lw := httptest.NewRecorder()
	testHandler.ListChatMessages(lw, list)
	var listed []map[string]any
	if err := json.Unmarshal(lw.Body.Bytes(), &listed); err != nil || lw.Code != http.StatusOK {
		t.Fatalf("list: %d %s %v", lw.Code, lw.Body.String(), err)
	}
	check("list", listed)

	for name, query := range map[string]string{"page": "limit=10", "around": "limit=10&around=" + wakeupMessageID} {
		req := httptest.NewRequest(http.MethodGet, "/api/chat/sessions/"+sessionID+"/messages/page?"+query, nil)
		req.Header.Set("X-User-ID", testUserID)
		req = withChatTestWorkspaceCtx(t, withURLParam(req, "sessionId", sessionID))
		w := httptest.NewRecorder()
		testHandler.ListChatMessagesPage(w, req)
		var page struct {
			Messages []map[string]any `json:"messages"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s %v", name, w.Code, w.Body.String(), err)
		}
		check(name, page.Messages)
	}
}
