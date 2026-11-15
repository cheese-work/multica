package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMarkChatSessionUnread(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID := createHandlerTestAgent(t, "ChatMarkUnreadAgent", []byte("[]"))

	newSession := func(t *testing.T) string {
		t.Helper()
		sessionID := createHandlerTestChatSession(t, agentID)
		t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM chat_session WHERE id = $1`, sessionID) })
		return sessionID
	}
	addMessage := func(t *testing.T, sessionID, role, createdAt string) {
		t.Helper()
		if _, err := testPool.Exec(ctx,
			`INSERT INTO chat_message (chat_session_id, role, content, created_at) VALUES ($1, $2, 'm', $3::timestamptz)`,
			sessionID, role, createdAt); err != nil {
			t.Fatalf("insert %s message: %v", role, err)
		}
	}
	setCursor := func(t *testing.T, sessionID, at string) {
		t.Helper()
		if _, err := testPool.Exec(ctx, `UPDATE chat_session SET last_read_at = $2::timestamptz WHERE id = $1`, sessionID, at); err != nil {
			t.Fatalf("set last_read_at: %v", err)
		}
	}
	markUnread := func(t *testing.T, sessionID string) *httptest.ResponseRecorder {
		t.Helper()
		req := withChatTestWorkspaceCtx(t, withURLParam(newRequest(http.MethodPost, "/api/chat/sessions/"+sessionID+"/unread", nil), "sessionId", sessionID))
		w := httptest.NewRecorder()
		testHandler.MarkChatSessionUnread(w, req)
		return w
	}
	unreadOf := func(t *testing.T, sessionID string) int {
		t.Helper()
		w := httptest.NewRecorder()
		testHandler.ListChatSessions(w, withChatTestWorkspaceCtx(t, newRequest("GET", "/api/chat/sessions?status=all", nil)))
		var resp []ChatSessionResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		for _, s := range resp {
			if s.ID == sessionID {
				return s.UnreadCount
			}
		}
		t.Fatalf("session %s not in list", sessionID)
		return 0
	}

	t.Run("latest assistant message becomes the only unread one", func(t *testing.T) {
		sessionID := newSession(t)
		addMessage(t, sessionID, "assistant", "2026-01-01T00:00:00Z")
		addMessage(t, sessionID, "assistant", "2026-01-02T00:00:00Z")
		addMessage(t, sessionID, "user", "2026-01-03T00:00:00Z")
		setCursor(t, sessionID, "2026-01-04T00:00:00Z")
		if got := unreadOf(t, sessionID); got != 0 {
			t.Fatalf("baseline unread = %d, want 0", got)
		}
		if w := markUnread(t, sessionID); w.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204: %s", w.Code, w.Body.String())
		}
		if got := unreadOf(t, sessionID); got != 1 {
			t.Fatalf("unread after mark-unread = %d, want 1", got)
		}
	})

	t.Run("no assistant message is a no-op", func(t *testing.T) {
		sessionID := newSession(t)
		addMessage(t, sessionID, "user", "2026-01-01T00:00:00Z")
		setCursor(t, sessionID, "2026-01-04T00:00:00Z")
		if w := markUnread(t, sessionID); w.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204: %s", w.Code, w.Body.String())
		}
		var moved bool
		if err := testPool.QueryRow(ctx, `SELECT last_read_at <> '2026-01-04T00:00:00Z'::timestamptz FROM chat_session WHERE id = $1`, sessionID).Scan(&moved); err != nil {
			t.Fatal(err)
		}
		if moved {
			t.Fatal("last_read_at moved although the session has no assistant message")
		}
	})

	t.Run("archived session keeps unread 0", func(t *testing.T) {
		sessionID := newSession(t)
		addMessage(t, sessionID, "assistant", "2026-01-01T00:00:00Z")
		setCursor(t, sessionID, "2026-01-04T00:00:00Z")
		if _, err := testPool.Exec(ctx, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, sessionID); err != nil {
			t.Fatal(err)
		}
		markUnread(t, sessionID)
		if got := unreadOf(t, sessionID); got != 0 {
			t.Fatalf("archived unread = %d, want 0", got)
		}
	})

	t.Run("only the creator may mark unread", func(t *testing.T) {
		sessionID := newSession(t)
		addMessage(t, sessionID, "assistant", "2026-01-01T00:00:00Z")
		setCursor(t, sessionID, "2026-01-04T00:00:00Z")
		var otherID string
		if err := testPool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ('Chat Unread Other', 'chat-unread-other@multica.ai') RETURNING id`).Scan(&otherID); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, otherID) })
		if _, err := testPool.Exec(ctx, `UPDATE chat_session SET creator_id = $2 WHERE id = $1`, sessionID, otherID); err != nil {
			t.Fatal(err)
		}
		if w := markUnread(t, sessionID); w.Code == http.StatusNoContent {
			t.Fatalf("non-creator got 204")
		}
	})
}
