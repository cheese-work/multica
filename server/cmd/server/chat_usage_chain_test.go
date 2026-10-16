package main

// chat_usage_chain_test.go — CHE-1782 (L1b): the usage endpoint also reports a
// "whole conversation" total across the continuation chain. These tests go
// through the HTTP router, so they cover authorization and leakage as well as
// the arithmetic.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/auth"
)

type chatUsageChain struct {
	SessionCount   int                       `json:"session_count"`
	Partial        bool                      `json:"partial"`
	Tokens         chatUsageTokens           `json:"tokens"`
	UncostedTokens chatUsageTokens           `json:"uncosted_tokens"`
	CostUsdTicks   int64                     `json:"cost_usd_ticks"`
	ByModel        map[string]chatUsageModel `json:"by_model"`
	Runs           struct {
		Terminal   int   `json:"terminal"`
		Metered    int   `json:"metered"`
		Unreported int   `json:"unreported"`
		TotalRunMs int64 `json:"total_run_ms"`
	} `json:"runs"`
}

type chatUsageWithChain struct {
	chatUsageResponse
	Chain *chatUsageChain `json:"chain"`
}

func getChatUsageChain(t *testing.T, userID, sessionID string) (chatUsageWithChain, string) {
	t.Helper()
	status, body := chatRequestAs(t, userID, true, "/api/chat/sessions/"+sessionID+"/usage")
	if status != http.StatusOK {
		t.Fatalf("usage %s: %d %s", sessionID, status, body)
	}
	var got chatUsageWithChain
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	return got, string(body)
}

// chainSession inserts one continuation-chain member with one completed,
// metered run: input tokens = tokens, one second long, priced at tokens ticks
// when priced, on the given model. agentID "" picks the workspace's first non-private agent.
func chainSession(t *testing.T, creatorID, workspaceID, agentID, model string, tokens int64, priced bool) string {
	t.Helper()
	ctx := context.Background()
	var runtimeID string
	if err := testPool.QueryRow(ctx, `SELECT COALESCE(NULLIF($2, '')::uuid, id), runtime_id FROM agent WHERE workspace_id = $1 AND (($2 = '' AND visibility <> 'private') OR id::text = $2) ORDER BY created_at LIMIT 1`, testWorkspaceID, agentID).Scan(&agentID, &runtimeID); err != nil {
		t.Fatal(err)
	}
	var sessionID string
	if err := testPool.QueryRow(ctx, `INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, explicitly_created_at) VALUES ($1, $2, $3, 'chain', now()) RETURNING id`, workspaceID, agentID, creatorID).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE chat_session_id = $1`, sessionID)
		testPool.Exec(context.Background(), `DELETE FROM chat_session WHERE id = $1`, sessionID)
	})
	var taskID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, chat_session_id, status, started_at, completed_at)
		VALUES ($1, $2, $3, 'completed', now() - interval '1 second', now()) RETURNING id`, agentID, runtimeID, sessionID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	var cost *int64
	if priced {
		cost = &tokens
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO task_usage (task_id, provider, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_usd_ticks)
		VALUES ($1, 'p', $2, $3::bigint, $3::bigint * 2, $3::bigint * 3, $3::bigint * 4, $4)`, taskID, model, tokens, cost); err != nil {
		t.Fatal(err)
	}
	return sessionID
}

// linkContinuation makes next the continuation of prev, as ContinueChatSession does.
func linkContinuation(t *testing.T, prev, next string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `UPDATE chat_session SET continued_into_session_id = $2 WHERE id = $1`, prev, next); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(context.Background(), `UPDATE chat_session SET continued_from_session_id = $1 WHERE id = $2`, prev, next); err != nil {
		t.Fatal(err)
	}
}

// directChainSum is the reference: a plain SQL sum over task_usage for ids.
func directChainSum(t *testing.T, ids ...string) (tokens chatUsageTokens, cost int64) {
	t.Helper()
	if err := testPool.QueryRow(context.Background(), `
		SELECT COALESCE(SUM(tu.input_tokens),0), COALESCE(SUM(tu.output_tokens),0),
		       COALESCE(SUM(tu.cache_read_tokens),0), COALESCE(SUM(tu.cache_write_tokens),0),
		       COALESCE(SUM(tu.cost_usd_ticks),0)
		FROM task_usage tu JOIN agent_task_queue atq ON atq.id = tu.task_id
		WHERE atq.chat_session_id = ANY($1::uuid[])`, ids).Scan(&tokens.Input, &tokens.Output, &tokens.CacheRead, &tokens.CacheWrite, &cost); err != nil {
		t.Fatal(err)
	}
	return tokens, cost
}

func TestChatUsageChainTotalEqualsDirectSumAcrossTheChain(t *testing.T) {
	var secondAgent string
	if err := testPool.QueryRow(context.Background(), `SELECT id FROM agent WHERE workspace_id = $1 LIMIT 1`, testWorkspaceID).Scan(&secondAgent); err != nil {
		t.Fatal(err)
	}
	a := chainSession(t, testUserID, testWorkspaceID, "", "m-a", 10, true)
	b := chainSession(t, testUserID, testWorkspaceID, secondAgent, "m-b", 20, false) // an /agent handoff hop
	c := chainSession(t, testUserID, testWorkspaceID, "", "m-a", 40, true)
	linkContinuation(t, a, b)
	linkContinuation(t, b, c)
	// A fork of B is a sibling conversation, not a continuation: never counted.
	fork := chainSession(t, testUserID, testWorkspaceID, "", "m-fork", 1000, true)
	if _, err := testPool.Exec(context.Background(), `UPDATE chat_session SET forked_from_session_id = $1 WHERE id = $2`, b, fork); err != nil {
		t.Fatal(err)
	}

	wantTokens, wantCost := directChainSum(t, a, b, c)
	for _, viewed := range []string{a, b, c} {
		got, _ := getChatUsageChain(t, testUserID, viewed)
		if got.Chain == nil {
			t.Fatalf("viewing %s: chain = null, want the 3-session total", viewed)
		}
		if got.Chain.SessionCount != 3 || got.Chain.Partial {
			t.Errorf("viewing %s: session_count/partial = %d/%v, want 3/false", viewed, got.Chain.SessionCount, got.Chain.Partial)
		}
		if got.Chain.Tokens != wantTokens || got.Chain.CostUsdTicks != wantCost {
			t.Errorf("viewing %s: chain tokens/cost = %+v/%d, direct SQL sum = %+v/%d", viewed, got.Chain.Tokens, got.Chain.CostUsdTicks, wantTokens, wantCost)
		}
		// 20 input tokens on B carry no provider price; they are the only uncosted ones.
		if want := (chatUsageTokens{Input: 20, Output: 40, CacheRead: 60, CacheWrite: 80}); got.Chain.UncostedTokens != want {
			t.Errorf("viewing %s: chain uncosted_tokens = %+v, want %+v", viewed, got.Chain.UncostedTokens, want)
		}
		if got.Chain.Runs.Terminal != 3 || got.Chain.Runs.Metered != 3 || got.Chain.Runs.Unreported != 0 {
			t.Errorf("viewing %s: chain runs = %+v, want 3 terminal, 3 metered", viewed, got.Chain.Runs)
		}
		if got.Chain.Runs.TotalRunMs < 2900 || got.Chain.Runs.TotalRunMs > 3300 {
			t.Errorf("viewing %s: chain total_run_ms = %d, want ~3000", viewed, got.Chain.Runs.TotalRunMs)
		}
		wantModels := map[string]chatUsageModel{
			"m-a": {Tokens: chatUsageTokens{Input: 50, Output: 100, CacheRead: 150, CacheWrite: 200}, CostUsdTicks: 50},
			"m-b": {Tokens: chatUsageTokens{Input: 20, Output: 40, CacheRead: 60, CacheWrite: 80}},
		}
		if len(got.Chain.ByModel) != len(wantModels) {
			t.Errorf("viewing %s: chain by_model = %+v, want %+v", viewed, got.Chain.ByModel, wantModels)
		}
		for model, want := range wantModels {
			if got.Chain.ByModel[model] != want {
				t.Errorf("viewing %s: chain by_model[%s] = %+v, want %+v", viewed, model, got.Chain.ByModel[model], want)
			}
		}
	}

	// "This DM only" is unchanged: the top-level totals stay session-only.
	got, _ := getChatUsageChain(t, testUserID, b)
	if got.Tokens.Input != 20 || got.CostUsdTicks != 0 {
		t.Errorf("session-only totals for B = %+v/%d, want input 20, cost 0", got.Tokens, got.CostUsdTicks)
	}
}

func TestChatUsageChainIsNullWithoutAContinuationLink(t *testing.T) {
	lone := chainSession(t, testUserID, testWorkspaceID, "", "m-a", 5, true)
	got, body := getChatUsageChain(t, testUserID, lone)
	if got.Chain != nil {
		t.Errorf("chain = %+v for a DM with no continuation, want null", got.Chain)
	}
	if !strings.Contains(body, `"chain":null`) {
		t.Errorf("response must carry an explicit \"chain\": null, got %s", body)
	}
	// A dangling link (target deleted) is not a chain either.
	if _, err := testPool.Exec(context.Background(), `UPDATE chat_session SET continued_from_session_id = $1 WHERE id = $2`, uuid.NewString(), lone); err != nil {
		t.Fatal(err)
	}
	if got, _ := getChatUsageChain(t, testUserID, lone); got.Chain != nil {
		t.Errorf("chain = %+v for a dangling link, want null", got.Chain)
	}
}

func TestChatUsageChainStopsAtOneHundredHops(t *testing.T) {
	const length = 103
	ids := make([]string, length)
	for i := range ids {
		ids[i] = chainSession(t, testUserID, testWorkspaceID, "", "m-a", int64(i+1), true)
		if i > 0 {
			linkContinuation(t, ids[i-1], ids[i])
		}
	}

	// From the head the walk reaches 100 hops: sessions 0..100, then stops.
	head, _ := getChatUsageChain(t, testUserID, ids[0])
	wantTokens, wantCost := directChainSum(t, ids[:101]...)
	if head.Chain == nil || head.Chain.SessionCount != 101 || !head.Chain.Partial {
		t.Fatalf("head chain = %+v, want 101 sessions and partial", head.Chain)
	}
	if head.Chain.Tokens != wantTokens || head.Chain.CostUsdTicks != wantCost {
		t.Errorf("head chain tokens/cost = %+v/%d, direct sum over the first 101 = %+v/%d (sessions past the limit leaked in or out)", head.Chain.Tokens, head.Chain.CostUsdTicks, wantTokens, wantCost)
	}

	// From the middle the 100-hop cap is a total, not per direction: 100 hops
	// are spent breadth-first, 50 each way, so sessions 1..101 are in and the
	// two ends (0 and 102) are not.
	middle, _ := getChatUsageChain(t, testUserID, ids[51])
	midTokens, midCost := directChainSum(t, ids[1:102]...)
	if middle.Chain == nil || middle.Chain.SessionCount != 101 || !middle.Chain.Partial {
		t.Fatalf("middle chain = %+v, want 101 sessions (100 hops in total) and partial", middle.Chain)
	}
	if middle.Chain.Tokens != midTokens || middle.Chain.CostUsdTicks != midCost {
		t.Errorf("middle chain tokens/cost = %+v/%d, direct sum over sessions 1..101 = %+v/%d", middle.Chain.Tokens, middle.Chain.CostUsdTicks, midTokens, midCost)
	}
}

func TestChatUsageChainIsCompleteAtExactlyOneHundredHops(t *testing.T) {
	ids := make([]string, 101)
	for i := range ids {
		ids[i] = chainSession(t, testUserID, testWorkspaceID, "", "m-a", int64(i+1), true)
		if i > 0 {
			linkContinuation(t, ids[i-1], ids[i])
		}
	}
	allTokens, allCost := directChainSum(t, ids...)
	// From either end the walk is exactly 100 hops; from the middle it is 50 + 50.
	for _, viewed := range []int{0, 50, 100} {
		got, _ := getChatUsageChain(t, testUserID, ids[viewed])
		if got.Chain == nil || got.Chain.SessionCount != 101 || got.Chain.Partial {
			t.Fatalf("viewing session %d: chain = %+v, want all 101 sessions and not partial", viewed, got.Chain)
		}
		if got.Chain.Tokens != allTokens || got.Chain.CostUsdTicks != allCost {
			t.Errorf("viewing session %d: chain tokens/cost = %+v/%d, direct sum = %+v/%d", viewed, got.Chain.Tokens, got.Chain.CostUsdTicks, allTokens, allCost)
		}
	}
}

func TestChatUsageChainSurvivesACycle(t *testing.T) {
	a := chainSession(t, testUserID, testWorkspaceID, "", "m-a", 3, true)
	b := chainSession(t, testUserID, testWorkspaceID, "", "m-a", 4, true)
	linkContinuation(t, a, b)
	linkContinuation(t, b, a) // corrupt data: the chain loops back
	got, _ := getChatUsageChain(t, testUserID, a)
	wantTokens, _ := directChainSum(t, a, b)
	if got.Chain == nil || got.Chain.SessionCount != 2 || got.Chain.Tokens != wantTokens {
		t.Errorf("cyclic chain = %+v, want each session counted once (%+v)", got.Chain, wantTokens)
	}
}

func TestChatUsageChainNeverIncludesAnotherUsersOrWorkspacesSessions(t *testing.T) {
	ctx := context.Background()
	var otherUserID, otherWorkspaceID string
	if err := testPool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ('Chain Other', $1) RETURNING id`, uuid.NewString()+"@multica.ai").Scan(&otherUserID); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `INSERT INTO workspace (name, slug) VALUES ('Chain Other WS', $1) RETURNING id`, "chain-other-"+uuid.NewString()[:8]).Scan(&otherWorkspaceID); err != nil {
		t.Fatal(err)
	}
	own := chainSession(t, testUserID, testWorkspaceID, "", "m-own", 7, true)
	// Registered before the sessions' cleanups run, so those run first.
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM chat_session WHERE creator_id = $1 OR workspace_id = $2`, otherUserID, otherWorkspaceID)
		testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, otherWorkspaceID)
		testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, otherUserID)
	})
	foreignUser := chainSession(t, otherUserID, testWorkspaceID, "", "m-secret-user", 9000, true)
	foreignWorkspace := chainSession(t, testUserID, otherWorkspaceID, "", "m-secret-ws", 8000, true)
	beyond := chainSession(t, testUserID, testWorkspaceID, "", "m-beyond", 700, true)

	// own -> foreign user -> beyond, and own <- foreign workspace.
	linkContinuation(t, own, foreignUser)
	linkContinuation(t, foreignUser, beyond)
	linkContinuation(t, foreignWorkspace, own)

	got, body := getChatUsageChain(t, testUserID, own)
	if got.Chain != nil {
		t.Errorf("chain = %+v, want null: the only linked sessions belong to another user or workspace", got.Chain)
	}
	for _, secret := range []string{"m-secret-user", "m-secret-ws", "m-beyond"} {
		if strings.Contains(body, secret) {
			t.Errorf("response leaks %q from a session the caller cannot read: %s", secret, body)
		}
	}
}

func TestChatUsageChainLeavesOutSessionsOnAgentsTheCallerCannotAccess(t *testing.T) {
	ctx := context.Background()
	var ownerID, memberID, privateAgent, runtimeID string
	if err := testPool.QueryRow(ctx, `SELECT runtime_id FROM agent WHERE workspace_id = $1 LIMIT 1`, testWorkspaceID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	for _, u := range []struct {
		dst  *string
		name string
	}{{&ownerID, "Chain Private Owner"}, {&memberID, "Chain Plain Member"}} {
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
		VALUES ($1, 'chain-private-agent', '', 'cloud', '{}'::jsonb, $2, 'private', 1, $3, '', '{}'::jsonb, '[]'::jsonb)
		RETURNING id`, testWorkspaceID, runtimeID, ownerID).Scan(&privateAgent); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent WHERE id = $1`, privateAgent) })

	var ownAgent string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent (workspace_id, name, description, runtime_mode, runtime_config, runtime_id, visibility,
			max_concurrent_tasks, owner_id, instructions, custom_env, custom_args)
		VALUES ($1, 'chain-member-agent', '', 'cloud', '{}'::jsonb, $2, 'private', 1, $3, '', '{}'::jsonb, '[]'::jsonb)
		RETURNING id`, testWorkspaceID, runtimeID, memberID).Scan(&ownAgent); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent WHERE id = $1`, ownAgent) })

	// The member's chain: own agent -> someone else's private agent (no access) -> own agent.
	a := chainSession(t, memberID, testWorkspaceID, ownAgent, "m-a", 10, true)
	hidden := chainSession(t, memberID, testWorkspaceID, privateAgent, "m-hidden", 5000, true)
	c := chainSession(t, memberID, testWorkspaceID, ownAgent, "m-a", 40, true)
	linkContinuation(t, a, hidden)
	linkContinuation(t, hidden, c)

	got, body := getChatUsageChain(t, memberID, a)
	wantTokens, _ := directChainSum(t, a, c)
	if got.Chain == nil || got.Chain.SessionCount != 2 || got.Chain.Tokens != wantTokens {
		t.Fatalf("chain = %+v, want only the two readable sessions (%+v)", got.Chain, wantTokens)
	}
	if !got.Chain.Partial {
		t.Error("chain.partial = false, want true: a session in the chain was left out")
	}
	if strings.Contains(body, "m-hidden") {
		t.Errorf("response leaks the private-agent session's model: %s", body)
	}
}

func TestChatUsageChainIsNotShownToATaskToken(t *testing.T) {
	a := chainSession(t, testUserID, testWorkspaceID, "", "m-a", 10, true)
	b := chainSession(t, testUserID, testWorkspaceID, "", "m-secret-sibling", 20, true)
	linkContinuation(t, a, b)
	running := chatTaskToken(t, a, "running")

	// An agent running in A may read A's own usage, never the other sessions' numbers.
	status, body := chatRequestWithToken(t, running, "/api/chat/sessions/"+a+"/usage")
	if status != http.StatusOK {
		t.Fatalf("own-session usage = %d %s, want 200", status, body)
	}
	if !strings.Contains(string(body), `"chain":null`) || strings.Contains(string(body), "m-secret-sibling") {
		t.Errorf("task-token response must carry chain null and nothing from B: %s", body)
	}
	// An interactive caller still gets it.
	if got, _ := getChatUsageChain(t, testUserID, a); got.Chain == nil || got.Chain.SessionCount != 2 {
		t.Errorf("interactive chain = %+v, want 2 sessions", got.Chain)
	}
}

// requestQueryTrace records the SQL each HTTP request issues, keyed by the
// request id chi's RequestID middleware read from X-Request-Id.
type requestQueryTrace struct {
	mu    sync.Mutex
	byReq map[string][]string
}

var requestQueries = &requestQueryTrace{byReq: map[string][]string{}}

func (q *requestQueryTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if id := chimw.GetReqID(ctx); id != "" {
		q.mu.Lock()
		q.byReq[id] = append(q.byReq[id], d.SQL)
		q.mu.Unlock()
	}
	return ctx
}

func (q *requestQueryTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (q *requestQueryTrace) take(id string) []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	sqls := q.byReq[id]
	delete(q.byReq, id)
	return sqls
}

// usageStatements runs one interactive usage request as userID and returns the
// statements it issued and how long it took.
func usageStatements(t *testing.T, userID, sessionID string) ([]string, time.Duration) {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": userID, "amr": "interactive", "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString(auth.JWTSecret())
	if err != nil {
		t.Fatal(err)
	}
	id := "usage-" + uuid.NewString()
	req, err := http.NewRequest(http.MethodGet, testServer.URL+"/api/chat/sessions/"+sessionID+"/usage", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	req.Header.Set(chimw.RequestIDHeader, id)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("usage: %d %s", resp.StatusCode, body)
	}
	return requestQueries.take(id), elapsed
}

// chainMember inserts a plain workspace member.
func chainMember(t *testing.T, role string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := testPool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ('Chain Member', $1) RETURNING id`, uuid.NewString()+"@multica.ai").Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, id) })
	if _, err := testPool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, $3)`, testWorkspaceID, id, role); err != nil {
		t.Fatal(err)
	}
	return id
}

// chainAgent inserts an agent owned by ownerID. mode is the permission mode;
// target is "workspace", "member" (an allow-list entry for memberID) or "".
func chainAgent(t *testing.T, ownerID, mode, target, memberID string) string {
	t.Helper()
	ctx := context.Background()
	var runtimeID, agentID string
	if err := testPool.QueryRow(ctx, `SELECT runtime_id FROM agent WHERE workspace_id = $1 LIMIT 1`, testWorkspaceID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent (workspace_id, name, description, runtime_mode, runtime_config, runtime_id, visibility,
			max_concurrent_tasks, owner_id, instructions, custom_env, custom_args, permission_mode)
		VALUES ($1, $2, '', 'cloud', '{}'::jsonb, $3, 'workspace', 1, $4, '', '{}'::jsonb, '[]'::jsonb, $5)
		RETURNING id`, testWorkspaceID, "chain-agent-"+uuid.NewString()[:8], runtimeID, ownerID, mode).Scan(&agentID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_invocation_target WHERE agent_id = $1`, agentID)
		testPool.Exec(context.Background(), `DELETE FROM agent WHERE id = $1`, agentID)
	})
	switch target {
	case "workspace":
		if _, err := testPool.Exec(ctx, `INSERT INTO agent_invocation_target (agent_id, target_type, target_id) VALUES ($1, 'workspace', $2)`, agentID, testWorkspaceID); err != nil {
			t.Fatal(err)
		}
	case "member":
		if _, err := testPool.Exec(ctx, `INSERT INTO agent_invocation_target (agent_id, target_type, target_id) VALUES ($1, 'member', $2)`, agentID, memberID); err != nil {
			t.Fatal(err)
		}
	}
	return agentID
}

// chainOnAgents builds a chain with one session per agent and returns the
// first session id.
func chainOnAgents(t *testing.T, creatorID string, agents []string) string {
	t.Helper()
	ids := make([]string, len(agents))
	for i, agent := range agents {
		ids[i] = chainSession(t, creatorID, testWorkspaceID, agent, "m-a", int64(i+1), true)
		if i > 0 {
			linkContinuation(t, ids[i-1], ids[i])
		}
	}
	return ids[0]
}

func TestChatUsageChainAuthorizationQueriesDoNotGrowWithTheChain(t *testing.T) {
	member := chainMember(t, "member")
	owner := chainMember(t, "member")
	agentsFor := func(n int) []string {
		agents := make([]string, n)
		for i := range agents {
			// Each agent needs the invocation-target lookup to be readable.
			agents[i] = chainAgent(t, owner, "public_to", "workspace", "")
		}
		return agents
	}
	small := chainOnAgents(t, member, agentsFor(2))
	large := chainOnAgents(t, member, agentsFor(40))

	smallSQL, _ := usageStatements(t, member, small)
	largeSQL, elapsed := usageStatements(t, member, large)

	t.Logf("2 distinct agents: %d statements; 40 distinct agents: %d statements in %s", len(smallSQL), len(largeSQL), elapsed)
	if len(largeSQL) != len(smallSQL) {
		t.Errorf("a chain of 40 distinct agents issued %d statements, a chain of 2 issued %d: authorization queries grow with the chain (N+1)", len(largeSQL), len(smallSQL))
	}
	const budget = 14
	if len(largeSQL) > budget {
		t.Errorf("usage request issued %d statements for a 40-agent chain, want at most %d", len(largeSQL), budget)
	}
}

func TestChatUsageChainAgentReadabilityMatrix(t *testing.T) {
	// Which chain members each kind of viewer may include, via the same rules
	// as the single-session private-agent gate.
	owner := chainMember(t, "member")
	type viewerCase struct {
		name, role string
		readable   map[string]bool
	}
	for _, tc := range []viewerCase{
		{"plain member", "member", map[string]bool{"own": true, "workspace-target": true, "member-target": true, "private": false, "public-no-target": false}},
		{"workspace admin", "admin", map[string]bool{"own": true, "workspace-target": true, "member-target": true, "private": true, "public-no-target": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			viewer := chainMember(t, tc.role)
			agents := map[string]string{
				"own":              chainAgent(t, viewer, "private", "", ""),
				"workspace-target": chainAgent(t, owner, "public_to", "workspace", ""),
				"member-target":    chainAgent(t, owner, "public_to", "member", viewer),
				"private":          chainAgent(t, owner, "private", "", ""),
				"public-no-target": chainAgent(t, owner, "public_to", "", ""),
			}
			order := []string{"own", "workspace-target", "member-target", "private", "public-no-target"}
			list := make([]string, len(order))
			for i, k := range order {
				list[i] = agents[k]
			}
			head := chainOnAgents(t, viewer, list)
			// Parity with the single-session gate: a member session is in the
			// chain exactly when GET /api/chat/sessions/{id} lets the viewer read it.
			var memberSessions []string
			if err := testPool.QueryRow(context.Background(), `
				WITH RECURSIVE c AS (SELECT id, continued_into_session_id AS next FROM chat_session WHERE id = $1
				  UNION ALL SELECT s.id, s.continued_into_session_id FROM c JOIN chat_session s ON s.id = c.next)
				SELECT array_agg(id::text ) FROM c`, head).Scan(&memberSessions); err != nil {
				t.Fatal(err)
			}
			gateAllows := 0
			for _, id := range memberSessions {
				if status, _ := chatRequestAs(t, viewer, true, "/api/chat/sessions/"+id); status == http.StatusOK {
					gateAllows++
				}
			}
			// The viewed session's own agent must be readable (the session gate), so
			// view the first session and read the rest through the chain.
			got, _ := getChatUsageChain(t, viewer, head)
			want := 0
			for _, k := range order {
				if tc.readable[k] {
					want++
				}
			}
			if got.Chain == nil || got.Chain.SessionCount != want {
				t.Fatalf("chain = %+v, want %d readable sessions", got.Chain, want)
			}
			if gateAllows != want {
				t.Errorf("single-session gate allows %d of %d sessions, chain includes %d: the two must agree", gateAllows, len(memberSessions), want)
			}
			if wantPartial := want < len(order); got.Chain.Partial != wantPartial {
				t.Errorf("chain.partial = %v, want %v", got.Chain.Partial, wantPartial)
			}
		})
	}
}

// postContinue continues sessionID as userID through the real endpoint and
// returns the new session's id.
func postContinue(t *testing.T, userID, sessionID string) string {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": userID, "amr": "interactive", "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString(auth.JWTSecret())
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, testServer.URL+"/api/chat/sessions/"+sessionID+"/continue", strings.NewReader(`{"summary":"Carried over."}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("continue %s: %d %s", sessionID, resp.StatusCode, body)
	}
	var out struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	next := out.Session.ID
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM chat_message WHERE chat_session_id = $1`, next)
		testPool.Exec(context.Background(), `DELETE FROM chat_session WHERE id = $1`, next)
	})
	return next
}

// commandOnlyChannelSession turns a chain member into what a channel session
// looks like once its binding is retired: channel-origin, never explicitly
// created by a member, holding only a control-plane command (not a public
// chat, GetPublicChatSessionInWorkspace).
func commandOnlyChannelSession(t *testing.T, sessionID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `UPDATE chat_session SET explicitly_created_at = NULL, channel_origin_at = now() WHERE id = $1`, sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `DELETE FROM chat_message WHERE chat_session_id = $1`, sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO chat_message (chat_session_id, role, content, message_kind) VALUES ($1, 'user', '/new', 'channel_command')`, sessionID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM chat_message WHERE chat_session_id = $1`, sessionID)
	})
}

func TestChatUsageChainLeavesOutACommandOnlyChannelSessionContinuedIntoAVisibleDM(t *testing.T) {
	hidden := chainSession(t, testUserID, testWorkspaceID, "", "m-hidden-channel", 9000, true)
	commandOnlyChannelSession(t, hidden)
	// The source is not a public chat: its own session GET and usage are refused.
	if status, _ := chatRequestAs(t, testUserID, true, "/api/chat/sessions/"+hidden); status != http.StatusNotFound {
		t.Fatalf("session GET of a command-only channel session = %d, want 404", status)
	}
	// Continue still works on it once the binding is gone (it passes only the
	// ownership gate), which is how its usage reaches a visible successor.
	visible := postContinue(t, testUserID, hidden)
	tail := chainSession(t, testUserID, testWorkspaceID, "", "m-tail", 7, true)
	linkContinuation(t, visible, tail)

	got, body := getChatUsageChain(t, testUserID, visible)
	wantTokens, _ := directChainSum(t, tail)
	if got.Chain == nil || got.Chain.SessionCount != 2 || !got.Chain.Partial {
		t.Fatalf("chain = %+v, want the visible DM and its tail (2 sessions), partial", got.Chain)
	}
	if got.Chain.Tokens != wantTokens {
		t.Errorf("chain tokens = %+v, want only the public sessions' %+v", got.Chain.Tokens, wantTokens)
	}
	if strings.Contains(body, "m-hidden-channel") {
		t.Errorf("response leaks the command-only session's model: %s", body)
	}
}

func TestChatUsageChainKeepsAChannelSessionOnceItHoldsAPublicMessage(t *testing.T) {
	// The same shape, but the channel-origin source also holds a normal
	// message, so it is a public chat and belongs in the total.
	source := chainSession(t, testUserID, testWorkspaceID, "", "m-source", 40, true)
	commandOnlyChannelSession(t, source)
	if _, err := testPool.Exec(context.Background(), `INSERT INTO chat_message (chat_session_id, role, content, message_kind) VALUES ($1, 'user', 'hello', 'message')`, source); err != nil {
		t.Fatal(err)
	}
	visible := postContinue(t, testUserID, source)
	got, _ := getChatUsageChain(t, testUserID, visible)
	wantTokens, _ := directChainSum(t, source)
	if got.Chain == nil || got.Chain.SessionCount != 2 || got.Chain.Partial {
		t.Fatalf("chain = %+v, want both sessions and not partial", got.Chain)
	}
	if got.Chain.Tokens != wantTokens {
		t.Errorf("chain tokens = %+v, want the public source's %+v", got.Chain.Tokens, wantTokens)
	}
}
