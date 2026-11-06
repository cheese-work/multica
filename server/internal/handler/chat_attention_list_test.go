package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// CHE-4148 (L4): the session list carries the DM attention projection (plan r4).
func TestListChatSessionsAttention(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID := createHandlerTestAgent(t, "ChatAttentionListAgent", []byte("[]"))
	now := time.Now()

	newSession := func(t *testing.T) string {
		t.Helper()
		id := createHandlerTestChatSession(t, agentID)
		t.Cleanup(func() {
			testPool.Exec(ctx, `DELETE FROM chat_attention_item WHERE chain_root_session_id = $1`, id)
		})
		return id
	}
	addItem := func(t *testing.T, sessionID, kind string, openedAgo time.Duration, dueIn *time.Duration) {
		t.Helper()
		var due *time.Time
		if dueIn != nil {
			d := now.Add(*dueIn)
			due = &d
		}
		var agent *string
		if kind == "ask" {
			agent = &agentID
		}
		if _, err := testPool.Exec(ctx, `
			INSERT INTO chat_attention_item (workspace_id, chain_root_session_id, chat_session_id, user_id, agent_id, kind, opened_at, due_at)
			VALUES ($1, $2, $2, $3, $4, $5, $6, $7)`,
			testWorkspaceID, sessionID, testUserID, agent, kind, now.Add(-openedAgo), due); err != nil {
			t.Fatalf("insert %s item: %v", kind, err)
		}
	}
	list := func(t *testing.T, sessionID string) ChatSessionResponse {
		t.Helper()
		w := httptest.NewRecorder()
		testHandler.ListChatSessions(w, withChatTestWorkspaceCtx(t, newRequest("GET", "/api/chat/sessions?status=all", nil)))
		if w.Code != http.StatusOK {
			t.Fatalf("list status = %d: %s", w.Code, w.Body.String())
		}
		var resp []ChatSessionResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		for _, s := range resp {
			if s.ID == sessionID {
				return s
			}
		}
		t.Fatalf("session %s not in list", sessionID)
		return ChatSessionResponse{}
	}
	near := func(t *testing.T, name string, got *string, want time.Time) {
		t.Helper()
		if got == nil {
			t.Fatalf("%s = nil, want ~%s", name, want)
		}
		at, err := time.Parse(time.RFC3339Nano, *got)
		if err != nil {
			t.Fatalf("%s %q: %v", name, *got, err)
		}
		if d := at.Sub(want); d > time.Second || d < -time.Second {
			t.Fatalf("%s = %s, want ~%s", name, at, want)
		}
	}

	t.Run("A14 fresh, yellow at 4 h, red at 24 h with next_change_at", func(t *testing.T) {
		fresh, yellow, red := newSession(t), newSession(t), newSession(t)
		addItem(t, fresh, "ask", time.Hour, nil)
		addItem(t, yellow, "ask", 5*time.Hour, nil)
		addItem(t, red, "ask", 25*time.Hour, nil)

		got := list(t, fresh)
		if got.StaleLevel != "fresh" || got.AttentionSource == nil || *got.AttentionSource != "ask" {
			t.Fatalf("fresh = level %q source %v", got.StaleLevel, got.AttentionSource)
		}
		near(t, "fresh attention_since", got.AttentionSince, now.Add(-time.Hour))
		near(t, "fresh next_change_at", got.NextChangeAt, now.Add(3*time.Hour)) // yellow at 4 h

		got = list(t, yellow)
		if got.StaleLevel != "yellow" {
			t.Fatalf("yellow level = %q", got.StaleLevel)
		}
		near(t, "yellow next_change_at", got.NextChangeAt, now.Add(19*time.Hour)) // red at 24 h

		got = list(t, red)
		if got.StaleLevel != "red" || got.NextChangeAt != nil {
			t.Fatalf("red = level %q next %v", got.StaleLevel, got.NextChangeAt)
		}
	})

	t.Run("no obligation is none with null source and since", func(t *testing.T) {
		got := list(t, newSession(t))
		if got.StaleLevel != "none" || got.AttentionSource != nil || got.AttentionSince != nil || got.NextChangeAt != nil {
			t.Fatalf("empty DM = %+v", got)
		}
	})

	t.Run("A14 archived chain is never coloured", func(t *testing.T) {
		id := newSession(t)
		addItem(t, id, "ask", 30*time.Hour, nil)
		if _, err := testPool.Exec(ctx, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		if got := list(t, id); got.StaleLevel != "none" || got.AttentionSource != nil {
			t.Fatalf("archived = %+v", got)
		}
	})

	t.Run("C1 next_change_at includes a future later due time", func(t *testing.T) {
		id := newSession(t)
		due := 2 * time.Hour
		addItem(t, id, "later", 0, &due)
		got := list(t, id)
		if got.StaleLevel != "none" || got.AttentionSource != nil {
			t.Fatalf("later before due = level %q source %v", got.StaleLevel, got.AttentionSource)
		}
		near(t, "later next_change_at", got.NextChangeAt, now.Add(due))
	})

	t.Run("A1 read and message fetch leave obligation and age unchanged", func(t *testing.T) {
		id := newSession(t)
		addItem(t, id, "ask", 5*time.Hour, nil)
		before := list(t, id)

		readReq := withChatTestWorkspaceCtx(t, withURLParam(newRequest(http.MethodPost, "/api/chat/sessions/"+id+"/read", nil), "sessionId", id))
		if w := httptest.NewRecorder(); true {
			testHandler.MarkChatSessionRead(w, readReq)
			if w.Code >= 300 {
				t.Fatalf("mark read = %d: %s", w.Code, w.Body.String())
			}
		}
		msgReq := withChatTestWorkspaceCtx(t, withURLParam(newRequest(http.MethodGet, "/api/chat/sessions/"+id+"/messages", nil), "sessionId", id))
		if w := httptest.NewRecorder(); true {
			testHandler.ListChatMessages(w, msgReq)
			if w.Code >= 300 {
				t.Fatalf("list messages = %d: %s", w.Code, w.Body.String())
			}
		}

		after := list(t, id)
		if before.StaleLevel != after.StaleLevel || before.AttentionSource == nil || after.AttentionSource == nil ||
			*before.AttentionSource != *after.AttentionSource ||
			before.AttentionSince == nil || after.AttentionSince == nil || *before.AttentionSince != *after.AttentionSince {
			t.Fatalf("attention changed by read: before %+v after %+v", before, after)
		}
	})
}
