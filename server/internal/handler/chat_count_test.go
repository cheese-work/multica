package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// CHE-9364: GET /api/chat/count is a count-only read of one member's DM
// progress cards, for flow metric 7 of the daily brief.
func TestGetChatCount(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	privateAgent, ownerID, memberID := privateAgentTestFixture(t)
	publicAgent := createHandlerTestAgent(t, "chat-count-public", []byte("[]"))
	chat := insertChatSessionAs(t, publicAgent, memberID)
	archived := insertChatSessionAs(t, publicAgent, memberID)
	other := insertChatSessionAs(t, publicAgent, testUserID)
	if _, err := testPool.Exec(ctx, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, archived); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	card := func(session, kind string, at time.Time) {
		t.Helper()
		if _, err := testPool.Exec(ctx, `
			INSERT INTO chat_message (chat_session_id, role, content, message_kind, created_at)
			VALUES ($1, 'assistant', 'Progress update: 1 issue', $2, $3)`, session, kind, at); err != nil {
			t.Fatalf("seed message: %v", err)
		}
	}
	card(chat, "progress_card", base.Add(10*time.Minute))     // counted
	card(chat, "progress_card", base.Add(20*time.Minute))     // counted
	card(archived, "progress_card", base.Add(30*time.Minute)) // counted: archived DM still delivered it
	card(chat, "progress_card", base.Add(-time.Hour))         // before since
	card(chat, "message", base.Add(15*time.Minute))           // other kind
	card(other, "progress_card", base.Add(15*time.Minute))    // another member's DM

	get := func(t *testing.T, asUser, source, agentID string, q url.Values) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		req := newRequestAs(asUser, http.MethodGet, "/api/chat/count?"+q.Encode(), nil)
		req.Header.Set("X-Actor-Source", source)
		if agentID != "" {
			req.Header.Set("X-Agent-ID", agentID)
		}
		rec := httptest.NewRecorder()
		testHandler.GetChatCount(rec, chatPendingCtxAs(t, req, asUser))
		var body map[string]any
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode %s: %v", rec.Body.String(), err)
			}
		}
		return rec, body
	}
	query := func(member string) url.Values {
		return url.Values{"member": {member}, "kind": {"progress-card"}, "since": {base.Format(time.RFC3339)}}
	}

	t.Run("member counts own cards", func(t *testing.T) {
		rec, got := get(t, memberID, "interactive_jwt", "", query(memberID))
		if rec.Code != http.StatusOK || got["count"] != float64(3) {
			t.Fatalf("status = %d body = %s, want count 3", rec.Code, rec.Body.String())
		}
	})

	t.Run("response carries count since until only", func(t *testing.T) {
		_, got := get(t, memberID, "interactive_jwt", "", query(memberID))
		for _, k := range []string{"count", "since", "until"} {
			if _, ok := got[k]; !ok {
				t.Errorf("missing %q in %v", k, got)
			}
			delete(got, k)
		}
		if len(got) != 0 {
			t.Errorf("unexpected fields: %v", got)
		}
	})

	t.Run("until bounds the window", func(t *testing.T) {
		q := query(memberID)
		q.Set("until", base.Add(25*time.Minute).Format(time.RFC3339))
		_, got := get(t, memberID, "interactive_jwt", "", q)
		if got["count"] != float64(2) {
			t.Fatalf("count = %v, want 2", got["count"])
		}
	})

	t.Run("window with no cards counts 0", func(t *testing.T) {
		q := query(memberID)
		q.Set("since", base.Add(time.Hour).Format(time.RFC3339))
		rec, got := get(t, memberID, "interactive_jwt", "", q)
		if rec.Code != http.StatusOK || got["count"] != float64(0) {
			t.Fatalf("status = %d body = %s, want count 0", rec.Code, rec.Body.String())
		}
	})

	t.Run("task token of the member's own agent counts on their behalf", func(t *testing.T) {
		// privateAgent is owned by ownerID; ownerID is a workspace member.
		rec, _ := get(t, testUserID, "task_token", privateAgent, query(ownerID))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("task token run for the member counts", func(t *testing.T) {
		rec, got := get(t, memberID, "task_token", publicAgent, query(memberID))
		if rec.Code != http.StatusOK || got["count"] != float64(3) {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
		}
	})

	// Deny cases.
	denied := []struct {
		name, user, source, agent, member string
		want                              int
	}{
		{"member naming another member", memberID, "interactive_jwt", "", testUserID, http.StatusForbidden},
		{"task token of an agent neither run for nor owned by the member", testUserID, "task_token", publicAgent, memberID, http.StatusForbidden},
		{"cli jwt", memberID, "cli_jwt", "", memberID, http.StatusForbidden},
		{"member that is not in the workspace", testUserID, "task_token", privateAgent, "00000000-0000-0000-0000-000000000001", http.StatusForbidden},
	}
	for _, tc := range denied {
		t.Run("deny: "+tc.name, func(t *testing.T) {
			rec, _ := get(t, tc.user, tc.source, tc.agent, query(tc.member))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}

	bad := map[string]func(url.Values){
		"missing member":     func(q url.Values) { q.Del("member") },
		"malformed member":   func(q url.Values) { q.Set("member", "nope") },
		"missing since":      func(q url.Values) { q.Del("since") },
		"malformed since":    func(q url.Values) { q.Set("since", "yesterday") },
		"unknown kind":       func(q url.Values) { q.Set("kind", "message") },
		"until before since": func(q url.Values) { q.Set("until", base.Add(-time.Hour).Format(time.RFC3339)) },
	}
	for name, mutate := range bad {
		t.Run("bad request: "+name, func(t *testing.T) {
			q := query(memberID)
			mutate(q)
			rec, _ := get(t, memberID, "interactive_jwt", "", q)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
		})
	}
}
