package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/auth"
)

type chatUsageTokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

type chatUsageResponse struct {
	SessionID      string          `json:"session_id"`
	Tokens         chatUsageTokens `json:"tokens"`
	UncostedTokens chatUsageTokens `json:"uncosted_tokens"`
	CostUsdTicks   int64           `json:"cost_usd_ticks"`
	Runs           struct {
		ByStatus   map[string]int `json:"by_status"`
		Terminal   int            `json:"terminal"`
		Metered    int            `json:"metered"`
		TotalRunMs int64          `json:"total_run_ms"`
	} `json:"runs"`
	Messages struct {
		ByRole map[string]int `json:"by_role"`
	} `json:"messages"`
	Models                 []string `json:"models"`
	ContinuedFromSessionID *string  `json:"continued_from_session_id"`
	ContinuedIntoSessionID *string  `json:"continued_into_session_id"`
	ForkedFromSessionID    *string  `json:"forked_from_session_id"`
}

// interactiveChatRequest sends an interactive-provenance request as the fixture
// user; chat read routes refuse JWTs without the interactive `amr` claim.
func interactiveChatRequest(t *testing.T, path string) (int, []byte) {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": testUserID, "amr": "interactive", "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString(auth.JWTSecret())
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodGet, testServer.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// seedChatUsageSession inserts one session owned by creatorID with runs and
// usage rows: 2 metered completed runs, 1 unmetered completed run, 1 failed
// run, plus a queued and a running run that only count by status.
func seedChatUsageSession(t *testing.T, creatorID string) (sessionID string) {
	t.Helper()
	ctx := context.Background()
	var agentID, runtimeID string
	if err := testPool.QueryRow(ctx, `SELECT id, runtime_id FROM agent WHERE workspace_id = $1 LIMIT 1`, testWorkspaceID).Scan(&agentID, &runtimeID); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, continued_from_session_id, forked_from_session_id)
		VALUES ($1, $2, $3, 'usage', $4, $5) RETURNING id`,
		testWorkspaceID, agentID, creatorID, uuid.NewString(), uuid.NewString()).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE chat_session_id = $1`, sessionID)
		testPool.Exec(ctx, `DELETE FROM chat_message WHERE chat_session_id = $1`, sessionID)
		testPool.Exec(ctx, `DELETE FROM chat_session WHERE id = $1`, sessionID)
	})

	run := func(status string, seconds int) string {
		var id string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO agent_task_queue (agent_id, runtime_id, chat_session_id, status, started_at, completed_at)
			VALUES ($1, $2, $3, $4,
				CASE WHEN $5 > 0 THEN now() - make_interval(secs => $5) END,
				CASE WHEN $5 > 0 THEN now() END)
			RETURNING id`, agentID, runtimeID, sessionID, status, seconds).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	usage := func(taskID, model string, in, out, cacheRead, cacheWrite int64, cost *int64) {
		if _, err := testPool.Exec(ctx, `
			INSERT INTO task_usage (task_id, provider, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_usd_ticks)
			VALUES ($1, 'p', $2, $3, $4, $5, $6, $7)`, taskID, model, in, out, cacheRead, cacheWrite, cost); err != nil {
			t.Fatal(err)
		}
	}
	ticks := func(v int64) *int64 { return &v }

	first := run("completed", 10)
	usage(first, "m-a", 100, 50, 10, 5, ticks(1000))
	second := run("completed", 20)
	usage(second, "m-b", 200, 80, 20, 10, nil)
	usage(second, "m-a", 7, 3, 0, 0, ticks(500))
	run("completed", 5)
	run("failed", 3)
	run("queued", 0)
	run("running", 0)

	for _, m := range []struct{ role, kind string }{
		{"user", "message"}, {"assistant", "message"}, {"user", "message"},
		{"assistant", "message"}, {"assistant", "message"}, {"user", "onboarding_kickoff"},
	} {
		if _, err := testPool.Exec(ctx, `INSERT INTO chat_message (chat_session_id, role, content, message_kind) VALUES ($1, $2, 'x', $3)`, sessionID, m.role, m.kind); err != nil {
			t.Fatal(err)
		}
	}
	return sessionID
}

func TestChatSessionUsage(t *testing.T) {
	sessionID := seedChatUsageSession(t, testUserID)

	status, body := interactiveChatRequest(t, "/api/chat/sessions/"+sessionID+"/usage")
	if status != http.StatusOK {
		t.Fatalf("usage: %d %s", status, body)
	}
	var got chatUsageResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}

	// The totals must equal a direct SQL sum over this session's task_usage.
	var want chatUsageTokens
	var wantCost int64
	if err := testPool.QueryRow(context.Background(), `
		SELECT COALESCE(SUM(tu.input_tokens),0), COALESCE(SUM(tu.output_tokens),0),
		       COALESCE(SUM(tu.cache_read_tokens),0), COALESCE(SUM(tu.cache_write_tokens),0),
		       COALESCE(SUM(tu.cost_usd_ticks),0)
		FROM task_usage tu JOIN agent_task_queue atq ON atq.id = tu.task_id
		WHERE atq.chat_session_id = $1`, sessionID).Scan(&want.Input, &want.Output, &want.CacheRead, &want.CacheWrite, &wantCost); err != nil {
		t.Fatal(err)
	}
	if got.Tokens != want || got.CostUsdTicks != wantCost {
		t.Errorf("tokens/cost = %+v/%d, direct SQL sum = %+v/%d", got.Tokens, got.CostUsdTicks, want, wantCost)
	}
	if want := (chatUsageTokens{Input: 307, Output: 133, CacheRead: 30, CacheWrite: 15}); got.Tokens != want {
		t.Errorf("tokens = %+v, want %+v", got.Tokens, want)
	}
	if want := (chatUsageTokens{Input: 200, Output: 80, CacheRead: 20, CacheWrite: 10}); got.UncostedTokens != want {
		t.Errorf("uncosted_tokens = %+v, want %+v", got.UncostedTokens, want)
	}
	if got.SessionID != sessionID {
		t.Errorf("session_id = %q, want %q", got.SessionID, sessionID)
	}
	for status, want := range map[string]int{"completed": 3, "failed": 1, "cancelled": 0, "queued": 1, "running": 1} {
		if got.Runs.ByStatus[status] != want {
			t.Errorf("runs.by_status[%s] = %d, want %d", status, got.Runs.ByStatus[status], want)
		}
	}
	if got.Runs.Terminal != 4 || got.Runs.Metered != 2 {
		t.Errorf("terminal/metered = %d/%d, want 4/2", got.Runs.Terminal, got.Runs.Metered)
	}
	if got.Runs.TotalRunMs < 38000 || got.Runs.TotalRunMs > 38100 {
		t.Errorf("total_run_ms = %d, want ~38000", got.Runs.TotalRunMs)
	}
	if got.Messages.ByRole["user"] != 2 || got.Messages.ByRole["assistant"] != 3 {
		t.Errorf("messages.by_role = %v, want user 2 assistant 3 (onboarding kickoff hidden)", got.Messages.ByRole)
	}
	if len(got.Models) != 2 || got.Models[0] != "m-a" || got.Models[1] != "m-b" {
		t.Errorf("models = %v, want [m-a m-b]", got.Models)
	}
	if got.ContinuedFromSessionID == nil || got.ForkedFromSessionID == nil || got.ContinuedIntoSessionID != nil {
		t.Errorf("continuation ids = %v/%v/%v", got.ContinuedFromSessionID, got.ContinuedIntoSessionID, got.ForkedFromSessionID)
	}
}

func TestChatSessionUsageRefusesForeignUserLikeSessionGet(t *testing.T) {
	var otherUserID string
	if err := testPool.QueryRow(context.Background(), `INSERT INTO "user" (name, email) VALUES ('Usage Other', $1) RETURNING id`, uuid.NewString()+"@multica.ai").Scan(&otherUserID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, otherUserID) })
	sessionID := seedChatUsageSession(t, otherUserID)

	wantStatus, wantBody := interactiveChatRequest(t, "/api/chat/sessions/"+sessionID)
	if wantStatus == http.StatusOK {
		t.Fatalf("session GET of a foreign session returned 200: %s", wantBody)
	}
	status, body := interactiveChatRequest(t, "/api/chat/sessions/"+sessionID+"/usage")
	if status != wantStatus || string(body) != string(wantBody) {
		t.Errorf("usage = %d %s, session GET = %d %s", status, body, wantStatus, wantBody)
	}
}
