package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
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

type chatUsageModel struct {
	Tokens       chatUsageTokens `json:"tokens"`
	CostUsdTicks int64           `json:"cost_usd_ticks"`
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
		Unreported int            `json:"unreported"`
		TotalRunMs int64          `json:"total_run_ms"`
	} `json:"runs"`
	Messages struct {
		ByRole map[string]int `json:"by_role"`
	} `json:"messages"`
	ByModel                map[string]chatUsageModel `json:"by_model"`
	ContinuedFromSessionID *string                   `json:"continued_from_session_id"`
	ContinuedIntoSessionID *string                   `json:"continued_into_session_id"`
	ForkedFromSessionID    *string                   `json:"forked_from_session_id"`
}

// chatRequestAs sends a GET as userID. An interactive caller carries the
// `amr: interactive` claim; chat read routes refuse JWTs without it.
func chatRequestAs(t *testing.T, userID string, interactive bool, path string) (int, []byte) {
	t.Helper()
	claims := jwt.MapClaims{"sub": userID, "exp": time.Now().Add(time.Hour).Unix()}
	if interactive {
		claims["amr"] = "interactive"
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(auth.JWTSecret())
	if err != nil {
		t.Fatal(err)
	}
	return chatRequestWithToken(t, token, path)
}

// chatRequestWithToken sends a GET with a raw bearer token (a JWT or a mat_
// task token) scoped to the fixture workspace.
func chatRequestWithToken(t *testing.T, token, path string) (int, []byte) {
	t.Helper()
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

func interactiveChatRequest(t *testing.T, path string) (int, []byte) {
	t.Helper()
	return chatRequestAs(t, testUserID, true, path)
}

// seedChatUsageSession inserts one session owned by creatorID with runs and
// usage rows: 2 metered completed runs, 1 unmetered completed run, 1 failed
// run, 1 failed run whose clock ran backwards, plus queued, running, deferred
// and waiting_local_directory runs that only count by status.
func seedChatUsageSession(t *testing.T, creatorID string) (sessionID string) {
	return seedChatUsageSessionWithAgent(t, creatorID, "")
}

// seedChatUsageSessionWithAgent is seedChatUsageSession on a specific agent
// ("" picks the workspace's first agent).
func seedChatUsageSessionWithAgent(t *testing.T, creatorID, wantAgentID string) (sessionID string) {
	t.Helper()
	ctx := context.Background()
	var agentID, runtimeID string
	if err := testPool.QueryRow(ctx, `SELECT id, runtime_id FROM agent WHERE workspace_id = $1 AND ($2 = '' OR id::text = $2) LIMIT 1`, testWorkspaceID, wantAgentID).Scan(&agentID, &runtimeID); err != nil {
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
		// A negative duration is a run whose completed_at precedes started_at.
		var id string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO agent_task_queue (agent_id, runtime_id, chat_session_id, status, started_at, completed_at)
			VALUES ($1, $2, $3, $4,
				CASE WHEN $5 <> 0 THEN now() - make_interval(secs => GREATEST($5, 0)) END,
				CASE WHEN $5 <> 0 THEN now() - make_interval(secs => GREATEST(-$5, 0)) END)
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
	run("failed", -50)
	run("queued", 0)
	run("running", 0)
	run("dispatched", 0)
	run("waiting_local_directory", 0)

	for _, m := range []struct{ role, kind string }{
		{"user", "message"}, {"assistant", "message"}, {"user", "message"},
		{"assistant", "message"}, {"assistant", "message"}, {"user", "onboarding_kickoff"},
		{"assistant", "onboarding_opening"}, {"user", "channel_command"},
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
	// by_status is an open map: statuses outside the five terminal/queue ones
	// (dispatched, waiting_local_directory, ...) must be preserved verbatim.
	wantStatus := map[string]int{"completed": 3, "failed": 2, "cancelled": 0, "queued": 1, "running": 1, "dispatched": 1, "waiting_local_directory": 1}
	for status, want := range wantStatus {
		if got.Runs.ByStatus[status] != want {
			t.Errorf("runs.by_status[%s] = %d, want %d", status, got.Runs.ByStatus[status], want)
		}
	}
	if len(got.Runs.ByStatus) != len(wantStatus) {
		t.Errorf("runs.by_status = %v, want exactly %v", got.Runs.ByStatus, wantStatus)
	}
	if got.Runs.Terminal != 5 || got.Runs.Metered != 2 || got.Runs.Unreported != 3 {
		t.Errorf("terminal/metered/unreported = %d/%d/%d, want 5/2/3", got.Runs.Terminal, got.Runs.Metered, got.Runs.Unreported)
	}
	if got.Runs.TotalRunMs < 38000 || got.Runs.TotalRunMs > 38100 {
		t.Errorf("total_run_ms = %d, want ~38000 (the backwards-clock run clamps to 0, not -50000)", got.Runs.TotalRunMs)
	}
	if len(got.Messages.ByRole) != 2 || got.Messages.ByRole["user"] != 2 || got.Messages.ByRole["assistant"] != 3 {
		t.Errorf("messages.by_role = %v, want only message_kind 'message' rows: user 2 assistant 3", got.Messages.ByRole)
	}
	wantModels := map[string]chatUsageModel{
		"m-a": {Tokens: chatUsageTokens{Input: 107, Output: 53, CacheRead: 10, CacheWrite: 5}, CostUsdTicks: 1500},
		"m-b": {Tokens: chatUsageTokens{Input: 200, Output: 80, CacheRead: 20, CacheWrite: 10}},
	}
	if len(got.ByModel) != len(wantModels) {
		t.Errorf("by_model = %+v, want %+v", got.ByModel, wantModels)
	}
	for model, want := range wantModels {
		if got.ByModel[model] != want {
			t.Errorf("by_model[%s] = %+v, want %+v", model, got.ByModel[model], want)
		}
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

// The usage route sits behind chatReadSession, so a caller GET /sessions/{id}
// refuses must be refused identically here.
func TestChatSessionUsageRefusesCLIJWTWithInteractiveLoginRequired(t *testing.T) {
	sessionID := seedChatUsageSession(t, testUserID)

	wantStatus, wantBody := chatRequestAs(t, testUserID, false, "/api/chat/sessions/"+sessionID)
	if wantStatus != http.StatusForbidden || !strings.Contains(string(wantBody), "interactive_login_required") {
		t.Fatalf("session GET with a non-interactive JWT = %d %s, want 403 interactive_login_required", wantStatus, wantBody)
	}
	status, body := chatRequestAs(t, testUserID, false, "/api/chat/sessions/"+sessionID+"/usage")
	if status != wantStatus || string(body) != string(wantBody) {
		t.Errorf("usage = %d %s, session GET = %d %s", status, body, wantStatus, wantBody)
	}
}

func TestChatSessionUsageRefusesPrivateAgentCreatorWithoutAccess(t *testing.T) {
	ctx := context.Background()
	var memberID, ownerID, agentID, runtimeID string
	if err := testPool.QueryRow(ctx, `SELECT runtime_id FROM agent WHERE workspace_id = $1 LIMIT 1`, testWorkspaceID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	for _, u := range []struct {
		dst  *string
		name string
	}{{&ownerID, "Usage Private Owner"}, {&memberID, "Usage Plain Member"}} {
		if err := testPool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id`, u.name, uuid.NewString()+"@multica.ai").Scan(u.dst); err != nil {
			t.Fatal(err)
		}
		id := *u.dst
		t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, id) })
		if _, err := testPool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'member')`, testWorkspaceID, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent (workspace_id, name, description, runtime_mode, runtime_config, runtime_id, visibility,
			max_concurrent_tasks, owner_id, instructions, custom_env, custom_args)
		VALUES ($1, 'usage-private-agent', '', 'cloud', '{}'::jsonb, $2, 'private', 1, $3, '', '{}'::jsonb, '[]'::jsonb)
		RETURNING id`, testWorkspaceID, runtimeID, ownerID).Scan(&agentID); err != nil {
		t.Fatal(err)
	}
	// Registered after the session cleanup below runs (LIFO), so the sessions go first.
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent WHERE id = $1`, agentID) })
	// The plain member created this session, so only the private-agent gate can refuse it.
	sessionID := seedChatUsageSessionWithAgent(t, memberID, agentID)

	wantStatus, wantBody := chatRequestAs(t, memberID, true, "/api/chat/sessions/"+sessionID)
	if wantStatus != http.StatusForbidden {
		t.Fatalf("session GET of a private-agent session by a plain member = %d %s, want 403", wantStatus, wantBody)
	}
	status, body := chatRequestAs(t, memberID, true, "/api/chat/sessions/"+sessionID+"/usage")
	if status != wantStatus || string(body) != string(wantBody) {
		t.Errorf("usage = %d %s, session GET = %d %s", status, body, wantStatus, wantBody)
	}
}

// usageRefusalMatchesSessionGet asserts a refusal: GET /sessions/{id} must not
// succeed with token, and /usage must answer with the same status and body.
func usageRefusalMatchesSessionGet(t *testing.T, token, sessionID string, wantStatus int) {
	t.Helper()
	getStatus, getBody := chatRequestWithToken(t, token, "/api/chat/sessions/"+sessionID)
	if getStatus != wantStatus {
		t.Fatalf("session GET = %d %s, want %d", getStatus, getBody, wantStatus)
	}
	status, body := chatRequestWithToken(t, token, "/api/chat/sessions/"+sessionID+"/usage")
	if status != getStatus || string(body) != string(getBody) {
		t.Errorf("usage = %d %s, session GET = %d %s", status, body, getStatus, getBody)
	}
}

func interactiveToken(t *testing.T) string {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": testUserID, "amr": "interactive", "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString(auth.JWTSecret())
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestChatSessionUsageRefusesCrossWorkspaceLikeSessionGet(t *testing.T) {
	ctx := context.Background()
	sessionID := seedChatUsageSession(t, testUserID)
	var otherWorkspaceID string
	if err := testPool.QueryRow(ctx, `INSERT INTO workspace (name, slug) VALUES ('Usage Other WS', $1) RETURNING id`, "usage-other-"+uuid.NewString()[:8]).Scan(&otherWorkspaceID); err != nil {
		t.Fatal(err)
	}
	// The session row moves into the new workspace, so delete it before the workspace.
	// Delete its tasks first: deleting the session nulls their chat_session_id
	// (ON DELETE SET NULL), which would hide them from the seed's own cleanup and
	// leave active tasks on the shared agent for later tests.
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE chat_session_id = $1`, sessionID)
		testPool.Exec(context.Background(), `DELETE FROM chat_session WHERE id = $1`, sessionID)
		testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, otherWorkspaceID)
	})
	if _, err := testPool.Exec(ctx, `UPDATE chat_session SET workspace_id = $1 WHERE id = $2`, otherWorkspaceID, sessionID); err != nil {
		t.Fatal(err)
	}
	// The caller created the session but asks through a workspace it does not belong to.
	usageRefusalMatchesSessionGet(t, interactiveToken(t), sessionID, http.StatusNotFound)
}

func TestChatSessionUsageHidesNonPublicSessionLikeSessionGet(t *testing.T) {
	ctx := context.Background()
	sessionID := seedChatUsageSession(t, testUserID)
	// A session holding only a channel command is a control-plane record, not a
	// public chat (GetPublicChatSessionInWorkspace).
	if _, err := testPool.Exec(ctx, `DELETE FROM chat_message WHERE chat_session_id = $1`, sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO chat_message (chat_session_id, role, content, message_kind) VALUES ($1, 'user', 'x', 'channel_command')`, sessionID); err != nil {
		t.Fatal(err)
	}
	usageRefusalMatchesSessionGet(t, interactiveToken(t), sessionID, http.StatusNotFound)
}

// chatTaskToken mints a mat_ token bound to a task of the given status on
// sessionID, as the daemon would at claim time.
func chatTaskToken(t *testing.T, sessionID, taskStatus string) string {
	t.Helper()
	ctx := context.Background()
	var agentID, runtimeID, taskID string
	if err := testPool.QueryRow(ctx, `SELECT agent_id FROM chat_session WHERE id = $1`, sessionID).Scan(&agentID); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `SELECT runtime_id FROM agent WHERE id = $1`, agentID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `INSERT INTO agent_task_queue (agent_id, runtime_id, chat_session_id, status) VALUES ($1, $2, $3, $4) RETURNING id`, agentID, runtimeID, sessionID, taskStatus).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	token := "mat_usage_" + taskID
	if _, err := testPool.Exec(ctx, `
		INSERT INTO task_token (token_hash, task_id, agent_id, workspace_id, user_id, expires_at)
		VALUES ($1, $2, $3, $4, $5, now() + interval '1 hour')`, auth.HashToken(token), taskID, agentID, testWorkspaceID, testUserID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM task_token WHERE task_id = $1`, taskID)
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID)
	})
	return token
}

func TestChatSessionUsageTaskTokenIsBoundToItsOwnSession(t *testing.T) {
	own := seedChatUsageSession(t, testUserID)
	other := seedChatUsageSession(t, testUserID)
	running := chatTaskToken(t, own, "running")

	// A running task reads its own session through both routes.
	for _, path := range []string{"/api/chat/sessions/" + own, "/api/chat/sessions/" + own + "/usage"} {
		if status, body := chatRequestWithToken(t, running, path); status != http.StatusOK {
			t.Fatalf("own-session %s = %d %s, want 200", path, status, body)
		}
	}
	// It cannot read another session, and usage refuses exactly as session GET does.
	usageRefusalMatchesSessionGet(t, running, other, http.StatusForbidden)
	// A task that is no longer running loses access to its own session.
	usageRefusalMatchesSessionGet(t, chatTaskToken(t, own, "completed"), own, http.StatusForbidden)
}
