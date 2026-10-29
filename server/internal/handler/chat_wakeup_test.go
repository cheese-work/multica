package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
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
