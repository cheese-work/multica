package main

// chat_usage_chain_test.go — CHE-1782 (L1b): the usage endpoint also reports a
// "whole conversation" total across the continuation chain. These tests go
// through the HTTP router, so they cover authorization and leakage as well as
// the arithmetic.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
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

	// From the middle, 100 hops reach both ends of 103 sessions: complete.
	middle, _ := getChatUsageChain(t, testUserID, ids[51])
	allTokens, allCost := directChainSum(t, ids...)
	if middle.Chain == nil || middle.Chain.SessionCount != length || middle.Chain.Partial {
		t.Fatalf("middle chain = %+v, want %d sessions and not partial", middle.Chain, length)
	}
	if middle.Chain.Tokens != allTokens || middle.Chain.CostUsdTicks != allCost {
		t.Errorf("middle chain tokens/cost = %+v/%d, direct sum = %+v/%d", middle.Chain.Tokens, middle.Chain.CostUsdTicks, allTokens, allCost)
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
