package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

type needsYouBody struct {
	AsOf         string         `json:"as_of"`
	Total        int            `json:"total"`
	ByKind       map[string]int `json:"by_kind"`
	OutsideChats map[string]int `json:"outside_chats"`
	Snoozed      map[string]int `json:"snoozed"`
	WithAgent    int            `json:"with_agent"`
	Unclassified int            `json:"unclassified"`
	NextChangeAt *string        `json:"next_change_at"`
}

// CHE-5670 slice 1: GET /api/needs-you projects one summary with the archived, hidden, unlinked
// and snoozed decisions from the plan's done-when seed.
func TestGetNeedsYou(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	privateAgent, _, memberID := privateAgentTestFixture(t)
	publicAgent := createHandlerTestAgent(t, "needs-you-public", []byte("[]"))
	issueID := createTestIssue(t, "needs-you issue", "todo", "none")
	t.Cleanup(func() { deleteTestIssue(t, issueID) })

	chatA := insertChatSessionAs(t, publicAgent, memberID)  // 2 decisions (1 snoozed) + 1 ask
	chatG := insertChatSessionAs(t, privateAgent, memberID) // hidden: 1 decision + 1 ask
	chatF := insertChatSessionAs(t, publicAgent, memberID)  // archived: 1 decision + 1 ask
	if _, err := testPool.Exec(ctx, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, chatF); err != nil {
		t.Fatal(err)
	}
	decision := func(chat, user string) string {
		return dbfx.Insert(t, "decision_request", testutil.Cols{
			"workspace_id": testWorkspaceID, "issue_id": issueID, "asking_agent_id": publicAgent, "human_user_id": user,
			"chat_session_id": chat, "escalation_class": "reversible", "origin": "asked",
		})
	}
	ask := func(chat, agent string) {
		dbfx.Insert(t, "chat_attention_item", testutil.Cols{
			"workspace_id": testWorkspaceID, "chain_root_session_id": chat, "chat_session_id": chat,
			"user_id": memberID, "kind": "ask", "agent_id": agent,
		})
	}
	decision(chatA, memberID)
	snoozed := decision(chatA, memberID)
	decision(chatG, memberID)
	decision(chatF, memberID)
	decision(uuid.NewString(), memberID) // chat row missing: unlinked
	decision(chatA, testUserID)          // another member's decision: never counted
	ask(chatA, publicAgent)
	ask(chatG, privateAgent)
	ask(chatF, publicAgent)
	snoozeEnd := time.Now().Add(time.Hour)
	if _, err := testPool.Exec(ctx, `
		INSERT INTO chat_attention_mark (user_id, subject_key, chain_root_session_id, snoozed_until) VALUES ($1, $2, $3, $4)`,
		memberID, service.DecisionKey(snoozed, 1), chatA, snoozeEnd); err != nil {
		t.Fatalf("insert snooze mark: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM chat_attention_mark WHERE user_id = $1`, memberID) })

	get := func(t *testing.T, asUser, source, query string) (*httptest.ResponseRecorder, needsYouBody) {
		t.Helper()
		req := newRequestAs(asUser, http.MethodGet, "/api/needs-you"+query, nil)
		req.Header.Set("X-Actor-Source", source)
		rec := httptest.NewRecorder()
		testHandler.GetNeedsYou(rec, chatPendingCtxAs(t, req, asUser))
		var body needsYouBody
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode %s: %v", rec.Body.String(), err)
			}
		}
		return rec, body
	}

	t.Run("member reads own summary", func(t *testing.T) {
		rec, got := get(t, memberID, "interactive_jwt", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		// decisions: A 1 (one snoozed), G hidden 1, F archived 1, unlinked 1 = 4; ask: A only = 1.
		wantKinds := map[string]int{"decision": 4, "ask": 1, "later": 0, "unseen": 0}
		for k, v := range wantKinds {
			if got.ByKind[k] != v {
				t.Errorf("by_kind[%s] = %d, want %d (%s)", k, got.ByKind[k], v, rec.Body.String())
			}
		}
		if got.Total != 5 {
			t.Errorf("total = %d, want 5", got.Total)
		}
		if got.OutsideChats["decision"] != 3 {
			t.Errorf("outside_chats.decision = %d, want 3", got.OutsideChats["decision"])
		}
		if got.Snoozed["decision"] != 1 {
			t.Errorf("snoozed.decision = %d, want 1", got.Snoozed["decision"])
		}
		if got.NextChangeAt == nil || got.AsOf == "" {
			t.Errorf("as_of=%q next_change_at=%v, want both set", got.AsOf, got.NextChangeAt)
		}
	})

	t.Run("response carries counts only", func(t *testing.T) {
		rec, _ := get(t, memberID, "interactive_jwt", "")
		var raw map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"as_of", "total", "by_kind", "outside_chats", "snoozed", "with_agent", "unclassified", "next_change_at"} {
			delete(raw, k)
		}
		if len(raw) != 0 {
			t.Errorf("unexpected fields: %v", raw)
		}
	})

	t.Run("member naming another member is forbidden", func(t *testing.T) {
		rec, _ := get(t, memberID, "interactive_jwt", "?for="+testUserID)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("member naming themselves is allowed", func(t *testing.T) {
		rec, got := get(t, memberID, "interactive_jwt", "?for="+memberID)
		if rec.Code != http.StatusOK || got.Total != 5 {
			t.Fatalf("status = %d total = %d", rec.Code, got.Total)
		}
	})

	t.Run("task token reads a named member", func(t *testing.T) {
		rec, got := get(t, testUserID, "task_token", "?for="+memberID)
		if rec.Code != http.StatusOK || got.Total != 5 || got.OutsideChats["decision"] != 3 {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("task token without for is a bad request", func(t *testing.T) {
		rec, _ := get(t, testUserID, "task_token", "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("malformed for is a bad request", func(t *testing.T) {
		rec, _ := get(t, memberID, "interactive_jwt", "?for=not-a-uuid")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
}
