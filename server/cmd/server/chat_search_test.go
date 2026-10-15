package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/auth"
)

// DM search is exercised through the real router: authorization is the whole
// point, so the route, auth middleware and SQL predicates are tested together.

type chatSearchHit struct {
	SessionID string  `json:"session_id"`
	Title     string  `json:"title"`
	AgentID   string  `json:"agent_id"`
	MessageID *string `json:"message_id"`
	Snippet   string  `json:"snippet"`
	UpdatedAt string  `json:"updated_at"`
	Archived  bool    `json:"archived"`
}

type chatSearchPage struct {
	Results    []chatSearchHit `json:"results"`
	NextCursor *string         `json:"next_cursor"`
}

type chatAroundPage struct {
	Messages []struct {
		ID      string `json:"id"`
		Content string `json:"content"`
	} `json:"messages"`
	HasMore    bool `json:"has_more"`
	HasNewer   bool `json:"has_newer"`
	NextCursor *struct {
		ID string `json:"id"`
	} `json:"next_cursor"`
}

func interactiveToken(t *testing.T, userID string) string {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": userID,
		"amr": "interactive",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}).SignedString(auth.JWTSecret())
	if err != nil {
		t.Fatalf("sign interactive token: %v", err)
	}
	return token
}

func mustDecode(t *testing.T, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
}

func chatGet(t *testing.T, token, path string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest("GET", testServer.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func searchChats(t *testing.T, userID, query string, extra ...string) chatSearchPage {
	t.Helper()
	path := "/api/chat/search?q=" + url.QueryEscape(query)
	for _, e := range extra {
		path += "&" + e
	}
	status, body := chatGet(t, interactiveToken(t, userID), path)
	if status != http.StatusOK {
		t.Fatalf("GET %s: status %d: %s", path, status, body)
	}
	var page chatSearchPage
	mustDecode(t, body, &page)
	return page
}

func searchSessionIDs(page chatSearchPage) []string {
	ids := make([]string, 0, len(page.Results))
	for _, hit := range page.Results {
		ids = append(ids, hit.SessionID)
	}
	return ids
}

// chatSearchFixture is a plain workspace member plus a public_to agent that
// the member may use, both owned by the fixture workspace owner (testUserID).
type chatSearchFixture struct {
	memberID      string
	publicAgentID string
	runtimeID     string
}

func newChatSearchFixture(t *testing.T) chatSearchFixture {
	t.Helper()
	ctx := context.Background()
	f := chatSearchFixture{}
	suffix := uuid.NewString()[:8]

	if err := testPool.QueryRow(ctx, `
		INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id
	`, "Chat Search Member", "chat-search-"+suffix+"@multica.test").Scan(&f.memberID); err != nil {
		t.Fatalf("create member user: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, f.memberID) })
	if _, err := testPool.Exec(ctx, `
		INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'member')
	`, testWorkspaceID, f.memberID); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if err := testPool.QueryRow(ctx, `
		SELECT id FROM agent_runtime WHERE workspace_id = $1 ORDER BY created_at LIMIT 1
	`, testWorkspaceID).Scan(&f.runtimeID); err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	f.publicAgentID = f.newAgent(t, "public_to")
	return f
}

func (f chatSearchFixture) newAgent(t *testing.T, permissionMode string) string {
	t.Helper()
	ctx := context.Background()
	var agentID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent (
			workspace_id, name, description, runtime_mode, runtime_config,
			runtime_id, visibility, max_concurrent_tasks, owner_id, permission_mode
		)
		VALUES ($1, $2, '', 'cloud', '{}'::jsonb, $3, 'workspace', 1, $4, $5)
		RETURNING id
	`, testWorkspaceID, "chat-search-agent-"+uuid.NewString()[:8], f.runtimeID, testUserID, permissionMode).Scan(&agentID); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM agent WHERE id = $1`, agentID) })
	if permissionMode == "public_to" {
		if _, err := testPool.Exec(ctx, `
			INSERT INTO agent_invocation_target (agent_id, target_type, target_id)
			VALUES ($1, 'workspace', $2)
		`, agentID, testWorkspaceID); err != nil {
			t.Fatalf("add invocation target: %v", err)
		}
	}
	return agentID
}

func (f chatSearchFixture) newSession(t *testing.T, creatorID, agentID, title, status string, explicit bool) string {
	t.Helper()
	ctx := context.Background()
	var sessionID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, status, explicitly_created_at)
		VALUES ($1, $2, $3, $4, $5, CASE WHEN $6 THEN now() END)
		RETURNING id
	`, testWorkspaceID, agentID, creatorID, title, status, explicit).Scan(&sessionID); err != nil {
		t.Fatalf("create chat session: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE chat_session_id = $1`, sessionID)
		testPool.Exec(ctx, `DELETE FROM chat_session WHERE id = $1`, sessionID)
	})
	return sessionID
}

func addChatMessage(t *testing.T, sessionID, role, kind, content string, at time.Time, taskID *string) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO chat_message (chat_session_id, role, content, message_kind, created_at, task_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id
	`, sessionID, role, content, kind, at, taskID).Scan(&id); err != nil {
		t.Fatalf("insert chat message: %v", err)
	}
	return id
}

func addChatTask(t *testing.T, f chatSearchFixture, sessionID, status string) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO agent_task_queue (agent_id, runtime_id, status, priority, chat_session_id)
		VALUES ($1, $2, $3, 0, $4)
		RETURNING id
	`, f.publicAgentID, f.runtimeID, status, sessionID).Scan(&id); err != nil {
		t.Fatalf("insert chat task: %v", err)
	}
	return id
}

func TestChatSearch_FindsMessageInReadableDM(t *testing.T) {
	f := newChatSearchFixture(t)
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	session := f.newSession(t, testUserID, f.publicAgentID, "Weekly planning", "active", true)
	addChatMessage(t, session, "user", "message", "hello there", base, nil)
	hitID := addChatMessage(t, session, "assistant", "message", "Here is the quarterly Zebra report you asked for", base.Add(time.Minute), nil)

	for _, q := range []string{"zebra", "ZEBRA"} {
		page := searchChats(t, testUserID, q)
		if len(page.Results) != 1 {
			t.Fatalf("q=%q: want 1 hit, got %+v", q, page.Results)
		}
		hit := page.Results[0]
		if hit.SessionID != session || hit.Title != "Weekly planning" || hit.AgentID != f.publicAgentID {
			t.Fatalf("q=%q: unexpected hit %+v", q, hit)
		}
		if hit.MessageID == nil || *hit.MessageID != hitID {
			t.Fatalf("q=%q: matched message = %v, want %s", q, hit.MessageID, hitID)
		}
		if !strings.Contains(strings.ToLower(hit.Snippet), "zebra") || hit.Archived || hit.UpdatedAt == "" {
			t.Fatalf("q=%q: bad snippet/flags %+v", q, hit)
		}
	}

	// LIKE wildcards in the query are literal text, not patterns.
	if page := searchChats(t, testUserID, "%"); len(page.Results) != 0 {
		t.Fatalf("q=%%: wildcard matched %+v", page.Results)
	}
}

func TestChatSearch_TitleMatchesRankFirstAndArchivedIsMarked(t *testing.T) {
	f := newChatSearchFixture(t)
	base := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	titleSession := f.newSession(t, testUserID, f.publicAgentID, "Walrus planning", "archived", true)
	addChatMessage(t, titleSession, "user", "message", "nothing relevant", base, nil)
	messageSession := f.newSession(t, testUserID, f.publicAgentID, "Other", "active", true)
	msgID := addChatMessage(t, messageSession, "user", "message", "ask about the walrus", base.Add(time.Hour), nil)

	page := searchChats(t, testUserID, "walrus")
	if got := searchSessionIDs(page); len(got) != 2 || got[0] != titleSession || got[1] != messageSession {
		t.Fatalf("order = %v, want title match %s first then %s", got, titleSession, messageSession)
	}
	if !page.Results[0].Archived || page.Results[0].MessageID != nil {
		t.Fatalf("title-only archived hit = %+v", page.Results[0])
	}
	if page.Results[1].MessageID == nil || *page.Results[1].MessageID != msgID {
		t.Fatalf("message hit = %+v", page.Results[1])
	}
}

func TestChatSearch_PaginatesWithCursor(t *testing.T) {
	f := newChatSearchFixture(t)
	base := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	a := f.newSession(t, testUserID, f.publicAgentID, "A", "active", true)
	b := f.newSession(t, testUserID, f.publicAgentID, "B", "active", true)
	addChatMessage(t, a, "user", "message", "pelican one", base, nil)
	addChatMessage(t, b, "user", "message", "pelican two", base.Add(time.Hour), nil)

	first := searchChats(t, testUserID, "pelican", "limit=1")
	if len(first.Results) != 1 || first.NextCursor == nil {
		t.Fatalf("first page = %+v", first)
	}
	second := searchChats(t, testUserID, "pelican", "limit=1", "cursor="+url.QueryEscape(*first.NextCursor))
	if len(second.Results) != 1 || second.NextCursor != nil || second.Results[0].SessionID == first.Results[0].SessionID {
		t.Fatalf("second page = %+v after %+v", second, first)
	}
}

func TestChatSearch_OnlyReturnsOwnDMs(t *testing.T) {
	f := newChatSearchFixture(t)
	session := f.newSession(t, f.memberID, f.publicAgentID, "Giraffe notes", "active", true)
	addChatMessage(t, session, "user", "message", "giraffe", time.Now(), nil)

	// The workspace owner can see the agent, but not another member's DM.
	if page := searchChats(t, testUserID, "giraffe"); len(page.Results) != 0 {
		t.Fatalf("owner saw member's DM: %+v", page.Results)
	}
	if page := searchChats(t, f.memberID, "giraffe"); len(page.Results) != 1 {
		t.Fatalf("creator did not see own DM: %+v", page.Results)
	}
}

func TestChatSearch_SkipsNonPublicSessions(t *testing.T) {
	f := newChatSearchFixture(t)
	session := f.newSession(t, testUserID, f.publicAgentID, "Okapi command log", "active", false)
	addChatMessage(t, session, "user", "channel_command", "/issue okapi", time.Now(), nil)

	if page := searchChats(t, testUserID, "okapi"); len(page.Results) != 0 {
		t.Fatalf("command-only session leaked: %+v", page.Results)
	}
}

func TestChatSearch_PrivateAgentAccessIsEnforced(t *testing.T) {
	f := newChatSearchFixture(t)
	session := f.newSession(t, f.memberID, f.publicAgentID, "Lemur", "active", true)
	addChatMessage(t, session, "user", "message", "lemur", time.Now(), nil)
	if page := searchChats(t, f.memberID, "lemur"); len(page.Results) != 1 {
		t.Fatalf("control: member should see DM with a public agent, got %+v", page.Results)
	}

	if _, err := testPool.Exec(context.Background(), `UPDATE agent SET permission_mode = 'private' WHERE id = $1`, f.publicAgentID); err != nil {
		t.Fatalf("make agent private: %v", err)
	}
	if page := searchChats(t, f.memberID, "lemur"); len(page.Results) != 0 {
		t.Fatalf("member kept access to a private agent's DM: %+v", page.Results)
	}

	private := f.newAgent(t, "private")
	ownerSession := f.newSession(t, testUserID, private, "Lemur owner", "active", true)
	addChatMessage(t, ownerSession, "user", "message", "lemur", time.Now(), nil)
	if page := searchChats(t, testUserID, "lemur"); len(page.Results) != 1 || page.Results[0].SessionID != ownerSession {
		t.Fatalf("agent owner should see own DM with private agent, got %+v", page.Results)
	}
}

func TestChatSearch_HiddenMessagesAreNeverReturned(t *testing.T) {
	f := newChatSearchFixture(t)
	base := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	session := f.newSession(t, testUserID, f.publicAgentID, "Plain title", "active", true)
	visible := addChatMessage(t, session, "user", "message", "narwhal visible", base, nil)

	// Hidden rows are newer than the visible one, so a missing filter would
	// pick them as the "most recent match".
	addChatMessage(t, session, "user", "channel_command", "/issue narwhal command", base.Add(time.Minute), nil)
	addChatMessage(t, session, "user", "onboarding_kickoff", "narwhal kickoff", base.Add(2*time.Minute), nil)
	head := addChatTask(t, f, session, "running")
	queued := addChatTask(t, f, session, "queued")
	addChatMessage(t, session, "user", "message", "narwhal queued follow-up", base.Add(3*time.Minute), &queued)
	addChatMessage(t, session, "user", "message", "narwhal running head", base.Add(-time.Minute), &head)

	page := searchChats(t, testUserID, "narwhal")
	if len(page.Results) != 1 {
		t.Fatalf("want 1 hit, got %+v", page.Results)
	}
	hit := page.Results[0]
	if hit.MessageID == nil || *hit.MessageID != visible {
		t.Fatalf("matched message = %v, want %s (snippet %q)", hit.MessageID, visible, hit.Snippet)
	}
	for _, hidden := range []string{"command", "kickoff", "queued"} {
		if strings.Contains(hit.Snippet, hidden) {
			t.Fatalf("snippet leaks hidden message: %q", hit.Snippet)
		}
	}

	// A session whose only match is hidden is not a hit at all.
	hiddenOnly := f.newSession(t, testUserID, f.publicAgentID, "Another title", "active", true)
	addChatMessage(t, hiddenOnly, "user", "onboarding_kickoff", "unicornsecret", base, nil)
	if page := searchChats(t, testUserID, "unicornsecret"); len(page.Results) != 0 {
		t.Fatalf("hidden-only session returned: %+v", page.Results)
	}
}

func TestChatSearch_RejectsNonInteractiveCallersAndEmptyQuery(t *testing.T) {
	status, body := chatGet(t, testToken, "/api/chat/search?q=anything") // cli_jwt
	if status != http.StatusForbidden {
		t.Fatalf("cli_jwt: status %d: %s", status, body)
	}
	status, body = chatGet(t, interactiveToken(t, testUserID), "/api/chat/search")
	if status != http.StatusBadRequest {
		t.Fatalf("missing q: status %d: %s", status, body)
	}
}

func TestChatSessionTitleHasTrigramIndex(t *testing.T) {
	var def string
	if err := testPool.QueryRow(context.Background(), `
		SELECT indexdef FROM pg_indexes
		WHERE tablename = 'chat_session' AND indexname = 'idx_chat_session_title_trgm'
	`).Scan(&def); err != nil {
		t.Fatalf("trigram index on chat_session.title missing: %v", err)
	}
	if !strings.Contains(def, "gin_trgm_ops") {
		t.Fatalf("unexpected index definition: %s", def)
	}
}

func seedAroundSession(t *testing.T, f chatSearchFixture) (string, []string) {
	t.Helper()
	base := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	session := f.newSession(t, testUserID, f.publicAgentID, "Around", "active", true)
	ids := make([]string, 0, 7)
	for i := 0; i < 7; i++ {
		ids = append(ids, addChatMessage(t, session, "user", "message", fmt.Sprintf("m%d", i), base.Add(time.Duration(i)*time.Second), nil))
		if i == 3 {
			// Hidden rows between visible ones must not appear in the window.
			addChatMessage(t, session, "user", "channel_command", "/issue hidden", base.Add(3*time.Second+500*time.Millisecond), nil)
		}
	}
	return session, ids
}

func fetchAround(t *testing.T, userID, path string) (int, chatAroundPage, []byte) {
	t.Helper()
	status, body := chatGet(t, interactiveToken(t, userID), path)
	var page chatAroundPage
	if status == http.StatusOK {
		mustDecode(t, body, &page)
	}
	return status, page, body
}

func TestChatMessages_AroundAnchorReturnsWindow(t *testing.T) {
	f := newChatSearchFixture(t)
	session, ids := seedAroundSession(t, f)

	for _, route := range []string{"messages", "messages/page"} {
		path := fmt.Sprintf("/api/chat/sessions/%s/%s?around=%s&limit=4", session, route, ids[3])
		status, page, body := fetchAround(t, testUserID, path)
		if status != http.StatusOK {
			t.Fatalf("%s: status %d: %s", route, status, body)
		}
		var got []string
		for _, m := range page.Messages {
			got = append(got, m.Content)
		}
		if strings.Join(got, ",") != "m1,m2,m3,m4" {
			t.Fatalf("%s: window = %v, want m1..m4", route, got)
		}
		if !page.HasMore || !page.HasNewer || page.NextCursor == nil || page.NextCursor.ID != ids[1] {
			t.Fatalf("%s: paging flags = %+v", route, page)
		}
	}

	// Anchor at the newest message: nothing newer, everything before is older.
	path := fmt.Sprintf("/api/chat/sessions/%s/messages/page?around=%s&limit=4", session, ids[6])
	_, page, _ := fetchAround(t, testUserID, path)
	if page.HasNewer || len(page.Messages) == 0 || page.Messages[len(page.Messages)-1].Content != "m6" {
		t.Fatalf("newest anchor page = %+v", page)
	}
}

func TestChatMessages_AroundAnchorRespectsAccessAndVisibility(t *testing.T) {
	f := newChatSearchFixture(t)
	session, ids := seedAroundSession(t, f)
	hidden := addChatMessage(t, session, "user", "channel_command", "/issue secret", time.Now(), nil)
	other := f.newSession(t, testUserID, f.publicAgentID, "Other", "active", true)
	otherMsg := addChatMessage(t, other, "user", "message", "elsewhere", time.Now(), nil)

	cases := map[string]struct {
		user, path string
		want       int
	}{
		"hidden anchor":             {testUserID, fmt.Sprintf("/api/chat/sessions/%s/messages/page?around=%s", session, hidden), http.StatusNotFound},
		"anchor in another session": {testUserID, fmt.Sprintf("/api/chat/sessions/%s/messages/page?around=%s", session, otherMsg), http.StatusNotFound},
		"unknown anchor":            {testUserID, fmt.Sprintf("/api/chat/sessions/%s/messages/page?around=%s", session, uuid.NewString()), http.StatusNotFound},
		"malformed anchor":          {testUserID, fmt.Sprintf("/api/chat/sessions/%s/messages/page?around=nope", session), http.StatusBadRequest},
		"not the creator":           {f.memberID, fmt.Sprintf("/api/chat/sessions/%s/messages/page?around=%s", session, ids[3]), http.StatusForbidden},
	}
	for name, c := range cases {
		status, _, body := fetchAround(t, c.user, c.path)
		if status != c.want {
			t.Errorf("%s: status %d, want %d: %s", name, status, c.want, body)
		}
		if strings.Contains(string(body), "secret") {
			t.Errorf("%s: body leaks hidden message: %s", name, body)
		}
	}
}
