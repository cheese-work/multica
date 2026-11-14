package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/llm"
)

// ---------------------------------------------------------------------------
// Test helpers for LLM chat auto-titling (MUL-4295)
// ---------------------------------------------------------------------------

// stubLLMCompletion returns an httptest server that mimics the OpenAI
// chat-completions endpoint, replying with `content` as the assistant message.
// When status != 200 it returns that status (with an error-ish body) so callers
// can exercise the upstream-failure fallback.
func stubLLMCompletion(t *testing.T, status int, content string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			// A retryable status still exercises the SDK's retries; Retry-After: 0
			// only stops them from sleeping through the backoff curve.
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"message":"stub upstream error"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		body := `{"id":"cmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":` + jsonString(content) + `},"finish_reason":"stop"}]}`
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// jsonString escapes s into a JSON string literal (including surrounding
// quotes) so titles containing quotes/newlines embed cleanly in the stub body.
func jsonString(s string) string {
	b := make([]byte, 0, len(s)+2)
	b = append(b, '"')
	for _, r := range s {
		switch r {
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		case '\n':
			b = append(b, '\\', 'n')
		case '\t':
			b = append(b, '\\', 't')
		default:
			b = append(b, string(r)...)
		}
	}
	b = append(b, '"')
	return string(b)
}

// withStubLLM points testHandler.LLM at a client backed by srv for the duration
// of the test, restoring the original (disabled) client afterwards.
func withStubLLM(t *testing.T, srv *httptest.Server) {
	t.Helper()
	prev := testHandler.LLM
	testHandler.LLM = llm.New(llm.Config{APIKey: "test-key", BaseURL: srv.URL})
	t.Cleanup(func() { testHandler.LLM = prev })
}

// chatTitleTestAgentID returns the seeded workspace test agent id.
func chatTitleTestAgentID(t *testing.T) pgtype.UUID {
	t.Helper()
	var agentID string
	if err := testPool.QueryRow(context.Background(),
		`SELECT id FROM agent WHERE workspace_id = $1 ORDER BY created_at ASC LIMIT 1`,
		testWorkspaceID,
	).Scan(&agentID); err != nil {
		t.Fatalf("load seeded agent: %v", err)
	}
	return parseUUID(agentID)
}

// newChatTitleTestSession creates a chat session with the given (original)
// title and returns its row. Cleaned up via t.Cleanup.
func newChatTitleTestSession(t *testing.T, title string) db.ChatSession {
	t.Helper()
	session, err := testHandler.Queries.CreateChatSession(context.Background(), db.CreateChatSessionParams{
		WorkspaceID: parseUUID(testWorkspaceID),
		AgentID:     chatTitleTestAgentID(t),
		CreatorID:   parseUUID(testUserID),
		Title:       title,
	})
	if err != nil {
		t.Fatalf("create chat session: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM chat_session WHERE id = $1`, uuidToString(session.ID))
	})
	return session
}

func chatSessionTitleFromDB(t *testing.T, sessionID pgtype.UUID) string {
	t.Helper()
	var title string
	if err := testPool.QueryRow(context.Background(),
		`SELECT title FROM chat_session WHERE id = $1`, uuidToString(sessionID),
	).Scan(&title); err != nil {
		t.Fatalf("load session title: %v", err)
	}
	return title
}

func requireDB(t *testing.T) {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
}

// ---------------------------------------------------------------------------
// sanitizeChatTitle unit tests: enforce the formatting rules regardless of how
// the model formats its reply (no quotes / no trailing punctuation / no label
// prefix / language-preserving / length cap).
// ---------------------------------------------------------------------------

func TestSanitizeChatTitle(t *testing.T) {
	longInput := ""
	for i := 0; i < chatSessionTitleMaxLen+50; i++ {
		longInput += "a"
	}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "Fix login bug", "Fix login bug"},
		{"surrounding double quotes", `"Fix login bug"`, "Fix login bug"},
		{"surrounding single quotes", `'Fix login bug'`, "Fix login bug"},
		{"smart quotes", "“修复登录问题”", "修复登录问题"},
		{"cjk brackets", "「优化查询性能」", "优化查询性能"},
		{"english label prefix", "Title: Fix login bug", "Fix login bug"},
		{"chinese label prefix", "标题：修复登录问题", "修复登录问题"},
		{"label then quotes", `标题："修复登录问题"`, "修复登录问题"},
		{"prefix wrapped in quotes", `"Title: Fix login"`, "Fix login"},
		{"prefix wrapped in cjk brackets", "「标题：修复登录问题」", "修复登录问题"},
		{"prefix in quotes with trailing period", `"Title: Fix login".`, "Fix login"},
		{"prefix in cjk brackets with trailing period", "「标题：修复登录问题」。", "修复登录问题"},
		{"trailing period", "Fix login bug.", "Fix login bug"},
		{"trailing cjk period", "修复登录问题。", "修复登录问题"},
		{"newlines collapsed", "Fix\nlogin\nbug", "Fix login bug"},
		{"leading trailing space", "   Fix login bug   ", "Fix login bug"},
		{"only punctuation empty", `"。"`, ""},
		{"blank", "   ", ""},
		{"length cap", longInput, longInput[:chatSessionTitleMaxLen]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeChatTitle(tc.in); got != tc.want {
				t.Fatalf("sanitizeChatTitle(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestShouldGenerateFirstMessageTitlePreservesChannelManualRename(t *testing.T) {
	if shouldGenerateFirstMessageTitle(false, "manual name", "", true, true) {
		t.Fatal("channel manual rename was treated as an auto-generated title")
	}
	if !shouldGenerateFirstMessageTitle(false, "derived title", "derived title", true, true) {
		t.Fatal("title initialized by the channel send should be eligible for refinement")
	}
	if shouldGenerateFirstMessageTitle(false, "unknown source", "", false, false) {
		t.Fatal("source lookup failure must skip optional title generation")
	}
	if !shouldGenerateFirstMessageTitle(false, "first-party seed", "", false, true) {
		t.Fatal("first-party Chat should retain the existing title refinement flow")
	}
}
