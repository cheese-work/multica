package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/governance"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/jev"
)

func TestParseSquadTierList(t *testing.T) {
	solID := dbid.NewV7()
	opusID := dbid.NewV7()
	lunaID := dbid.NewV7()
	unlistedID := dbid.NewV7()

	candidates := []db.Agent{
		{ID: solID, Name: "x99-codex-sol"},
		{ID: opusID, Name: "x99-claude-opus"},
		{ID: lunaID, Name: "x99-codex-luna-2"},
		{ID: unlistedID, Name: "unlisted-agent"},
	}

	markdownTable := `
# Squad Leader Instructions

## Specialist Tier Rankings
| Member | Tier | Specialization |
| x99-codex-sol | S | Go backend, distributed systems, SQL |
| x99-claude-opus | A | Architecture, code review, multi-turn reasoning |
| x99-codex-luna-2 | B | Android Maestro E2E test execution |
`

	tiers := parseSquadTierList(markdownTable, candidates)

	if got := tiers[util.UUIDToString(solID)]; got != "S" {
		t.Errorf("sol tier = %q, want S", got)
	}
	if got := tiers[util.UUIDToString(opusID)]; got != "A" {
		t.Errorf("opus tier = %q, want A", got)
	}
	if got := tiers[util.UUIDToString(lunaID)]; got != "B" {
		t.Errorf("luna tier = %q, want B", got)
	}
	if got := tiers[util.UUIDToString(unlistedID)]; got != "C" {
		t.Errorf("unlisted tier = %q, want default C", got)
	}

	bulletList := `
- x99-codex-sol: Tier S specialist for Go
- x99-claude-opus: Tier A reviewer
`
	tiersBullet := parseSquadTierList(bulletList, candidates)
	if got := tiersBullet[util.UUIDToString(solID)]; got != "S" {
		t.Errorf("bullet sol tier = %q, want S", got)
	}
	if got := tiersBullet[util.UUIDToString(opusID)]; got != "A" {
		t.Errorf("bullet opus tier = %q, want A", got)
	}
	if got := tiersBullet[util.UUIDToString(lunaID)]; got != "C" {
		t.Errorf("bullet luna tier = %q, want default C", got)
	}
}

func TestInstructionTokenLimits(t *testing.T) {
	// 3000 tokens limit for squad = ~9000 chars at 3.0 chars/token
	// 3001 tokens = ~9004 chars
	squadOverLimit := strings.Repeat("squad ", 2000) // ~12000 chars, ~4000 tokens
	if jev.EstimateTokens(squadOverLimit) <= 3000 {
		t.Fatalf("expected squadOverLimit to exceed 3000 tokens, got %d", jev.EstimateTokens(squadOverLimit))
	}

	squadUnderLimit := "Short squad instructions"
	if jev.EstimateTokens(squadUnderLimit) > 3000 {
		t.Fatalf("expected squadUnderLimit under 3000 tokens, got %d", jev.EstimateTokens(squadUnderLimit))
	}

	// 4000 tokens limit for agent = ~12000 chars at 3.0 chars/token
	agentOverLimit := strings.Repeat("agent ", 3000) // ~18000 chars, ~6000 tokens
	if jev.EstimateTokens(agentOverLimit) <= 4000 {
		t.Fatalf("expected agentOverLimit to exceed 4000 tokens, got %d", jev.EstimateTokens(agentOverLimit))
	}

	agentUnderLimit := "Short agent instructions"
	if jev.EstimateTokens(agentUnderLimit) > 4000 {
		t.Fatalf("expected agentUnderLimit under 4000 tokens, got %d", jev.EstimateTokens(agentUnderLimit))
	}
}

func TestJevRouteCommentSuppressesTriggers(t *testing.T) {
	h := &Handler{}
	issue := db.Issue{ID: dbid.NewV7()}
	comment := db.Comment{
		ID:         dbid.NewV7(),
		Content:    "⚡ Jev routed to Sol (Tier S) · Conf 0.94 · Reason: Go backend match · Decision jd_8f2c",
		Type:       "jev_route",
		AuthorType: "system",
	}

	outcomes := h.triggerTasksForComment(context.Background(), issue, comment, nil, "system", "", "", nil, nil)
	if outcomes != nil {
		t.Fatalf("triggerTasksForComment with type='jev_route' returned %v, want nil", outcomes)
	}
}

func TestSquadInstructionsUpdateTokenLimitHTTP(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}

	ctx := context.Background()
	var leaderID string
	if err := testPool.QueryRow(ctx, `
		SELECT id FROM agent WHERE workspace_id = $1 ORDER BY created_at ASC LIMIT 1
	`, testWorkspaceID).Scan(&leaderID); err != nil {
		t.Fatalf("load test agent: %v", err)
	}

	var squadID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO squad (workspace_id, name, description, leader_id, creator_id)
		VALUES ($1, $2, '', $3, $4)
		RETURNING id
	`, testWorkspaceID, "Token Limit Test Squad", leaderID, testUserID).Scan(&squadID); err != nil {
		t.Fatalf("create squad: %v", err)
	}
	defer testPool.Exec(ctx, `DELETE FROM squad WHERE id = $1`, squadID)

	overLimitInstructions := strings.Repeat("instruction token limit test ", 400) // ~11200 chars > 3000 tokens

	w := httptest.NewRecorder()
	req := newRequest("PATCH", "/api/squads/"+squadID+"?workspace_id="+testWorkspaceID, map[string]any{
		"instructions": overLimitInstructions,
	})
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("workspaceId", testWorkspaceID)
	rctx.URLParams.Add("id", squadID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	req.Header.Set("X-User-ID", testUserID)
	testHandler.UpdateSquad(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("UpdateSquad with over-limit instructions = %d, want 422 Unprocessable Entity. Body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "exceed limit of 3000 tokens") {
		t.Errorf("expected error message to mention 'exceed limit of 3000 tokens', got: %s", w.Body.String())
	}
}

func TestAgentInstructionsCreateTokenLimitHTTP(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}

	overLimitInstructions := strings.Repeat("agent instruction token limit test ", 500) // ~17500 chars > 4000 tokens

	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/agents?workspace_id="+testWorkspaceID, map[string]any{
		"name":         "OverLimit Agent",
		"instructions": overLimitInstructions,
		"runtime_id":   testRuntimeID,
	})
	req.Header.Set("X-User-ID", testUserID)
	testHandler.CreateAgent(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("CreateAgent with over-limit instructions = %d, want 422 Unprocessable Entity. Body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "exceed limit of 4000 tokens") {
		t.Errorf("expected error message to mention 'exceed limit of 4000 tokens', got: %s", w.Body.String())
	}
}

func TestSquadInstructionsGrandfatherRule(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}

	ctx := context.Background()
	var leaderID string
	if err := testPool.QueryRow(ctx, `
		SELECT id FROM agent WHERE workspace_id = $1 ORDER BY created_at ASC LIMIT 1
	`, testWorkspaceID).Scan(&leaderID); err != nil {
		t.Fatalf("load test agent: %v", err)
	}

	// 1. Insert squad directly with 4000 tokens of existing instructions (> 3000 limit)
	existingInstructions := strings.Repeat("grandfather squad instructions ", 400) // ~12400 chars, ~4000 tokens
	var squadID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO squad (workspace_id, name, description, instructions, leader_id, creator_id)
		VALUES ($1, 'Grandfather Squad', '', $2, $3, $4)
		RETURNING id
	`, testWorkspaceID, existingInstructions, leaderID, testUserID).Scan(&squadID); err != nil {
		t.Fatalf("create grandfather squad: %v", err)
	}
	defer testPool.Exec(ctx, `DELETE FROM squad WHERE id = $1`, squadID)

	// 2. Decreasing token count (e.g. 3500 tokens, still > 3000) should be allowed by grandfather rule
	smallerInstructions := strings.Repeat("grandfather squad instructions ", 350)
	w := httptest.NewRecorder()
	req := newRequest("PATCH", "/api/squads/"+squadID+"?workspace_id="+testWorkspaceID, map[string]any{
		"instructions": smallerInstructions,
	})
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("workspaceId", testWorkspaceID)
	rctx.URLParams.Add("id", squadID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	req.Header.Set("X-User-ID", testUserID)
	testHandler.UpdateSquad(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("UpdateSquad with smaller grandfathered instructions = %d, want 200 OK. Body: %s", w.Code, w.Body.String())
	}

	// 3. Increasing token count (e.g. 4500 tokens > previous 3500) should be rejected
	largerInstructions := strings.Repeat("grandfather squad instructions ", 450)
	w = httptest.NewRecorder()
	req = newRequest("PATCH", "/api/squads/"+squadID+"?workspace_id="+testWorkspaceID, map[string]any{
		"instructions": largerInstructions,
	})
	rctx = chi.NewRouteContext()
	rctx.URLParams.Add("workspaceId", testWorkspaceID)
	rctx.URLParams.Add("id", squadID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	req.Header.Set("X-User-ID", testUserID)
	testHandler.UpdateSquad(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("UpdateSquad increasing over-limit instructions = %d, want 422 Unprocessable Entity. Body: %s", w.Code, w.Body.String())
	}
}

func TestAgentInstructionsGrandfatherRule(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}

	ctx := context.Background()
	wsUUID, _ := util.ParseUUID(testWorkspaceID)
	rtUUID, _ := util.ParseUUID(testRuntimeID)
	userUUID, _ := util.ParseUUID(testUserID)

	// 1. Insert agent directly with 5000 tokens of existing instructions (> 4000 limit)
	existingInstructions := strings.Repeat("grandfather agent instructions ", 500) // ~15500 chars, ~5000 tokens
	agentID := dbid.NewV7()
	_, err := testPool.Exec(ctx, `
		INSERT INTO agent (id, workspace_id, runtime_id, owner_id, name, instructions, status, runtime_mode)
		VALUES ($1, $2, $3, $4, 'Grandfather Agent', $5, 'idle', 'cloud')
	`, agentID, wsUUID, rtUUID, userUUID, existingInstructions)
	if err != nil {
		t.Fatalf("create grandfather agent: %v", err)
	}
	defer testPool.Exec(ctx, `DELETE FROM agent WHERE id = $1`, agentID)

	agentIDStr := util.UUIDToString(agentID)

	// 2. Decreasing token count (e.g. 4500 tokens, still > 4000) should be allowed by grandfather rule
	smallerInstructions := strings.Repeat("grandfather agent instructions ", 450)
	w := httptest.NewRecorder()
	req := newRequest("PATCH", "/api/agents/"+agentIDStr+"?workspace_id="+testWorkspaceID, map[string]any{
		"instructions": smallerInstructions,
	})
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("workspaceId", testWorkspaceID)
	rctx.URLParams.Add("id", agentIDStr)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	req.Header.Set("X-User-ID", testUserID)
	testHandler.UpdateAgent(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("UpdateAgent with smaller grandfathered instructions = %d, want 200 OK. Body: %s", w.Code, w.Body.String())
	}

	// 3. Increasing token count (e.g. 5200 tokens > previous 4500) should be rejected
	largerInstructions := strings.Repeat("grandfather agent instructions ", 520)
	w = httptest.NewRecorder()
	req = newRequest("PATCH", "/api/agents/"+agentIDStr+"?workspace_id="+testWorkspaceID, map[string]any{
		"instructions": largerInstructions,
	})
	rctx = chi.NewRouteContext()
	rctx.URLParams.Add("workspaceId", testWorkspaceID)
	rctx.URLParams.Add("id", agentIDStr)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	req.Header.Set("X-User-ID", testUserID)
	testHandler.UpdateAgent(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("UpdateAgent increasing over-limit instructions = %d, want 422 Unprocessable Entity. Body: %s", w.Code, w.Body.String())
	}
}

func TestTryJevSquadRouting_FastPathAndFallback(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}

	ctx := context.Background()

	// 1. Create leader agent and member agent
	leaderAgentID := dbid.NewV7()
	memberAgentID := dbid.NewV7()
	wsUUID, _ := util.ParseUUID(testWorkspaceID)
	rtUUID, _ := util.ParseUUID(testRuntimeID)
	userUUID, _ := util.ParseUUID(testUserID)

	_, err := testPool.Exec(ctx, `
		INSERT INTO agent (id, workspace_id, runtime_id, owner_id, name, status, runtime_mode)
		VALUES ($1, $2, $3, $4, 'squad-leader-agent', 'idle', 'cloud'),
		       ($5, $2, $3, $4, 'squad-worker-sol', 'idle', 'cloud')
	`, leaderAgentID, wsUUID, rtUUID, userUUID, memberAgentID)
	if err != nil {
		t.Fatalf("insert test agents: %v", err)
	}
	defer testPool.Exec(ctx, `DELETE FROM agent WHERE id IN ($1, $2)`, leaderAgentID, memberAgentID)

	// 2. Create squad with leader and member
	squadID := dbid.NewV7()
	squadInstructions := `
## Specialist Tier Rankings
| Member | Tier | Specialization |
| squad-worker-sol | S | Go backend expert |
`
	_, err = testPool.Exec(ctx, `
		INSERT INTO squad (id, workspace_id, name, description, instructions, leader_id, creator_id)
		VALUES ($1, $2, 'Jev Test Squad', '', $3, $4, $5)
	`, squadID, wsUUID, squadInstructions, leaderAgentID, userUUID)
	if err != nil {
		t.Fatalf("insert squad: %v", err)
	}
	defer testPool.Exec(ctx, `DELETE FROM squad WHERE id = $1`, squadID)

	_, err = testPool.Exec(ctx, `
		INSERT INTO squad_member (id, squad_id, member_type, member_id, role)
		VALUES ($1, $2, 'agent', $3, 'worker')
	`, dbid.NewV7(), squadID, memberAgentID)
	if err != nil {
		t.Fatalf("insert squad member: %v", err)
	}

	// 3. Setup mock Jev server
	var returnConfidence = 0.95
	var returnChoice = util.UUIDToString(memberAgentID)

	mockJevServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probs := map[string]float64{
			returnChoice:         returnConfidence,
			"escalate_to_leader": 1.0 - returnConfidence,
		}
		resp := jev.Response{
			Model: "jev-1.13.0",
			Answers: map[string]jev.Answer{
				"route_squad_member": {
					Type:          jev.TypeChoice,
					Choice:        returnChoice,
					Confidence:    returnConfidence,
					Probabilities: probs,
				},
			},
			Usage: jev.Usage{InputTokens: 500, OutputTokens: 10},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockJevServer.Close()

	// 4. Configure workspace settings for Jev squad routing
	var minConf = 0.90
	settings := governance.WorkspaceSettings{
		JevSquadRoutingEnabled: true,
		JevAPIURL:              mockJevServer.URL,
		JevMinConfidence:       &minConf,
	}
	settingsJSON, _ := json.Marshal(settings)
	_, err = testPool.Exec(ctx, `
		INSERT INTO governance_workspace_config (workspace_id, config_version, control_epoch, settings)
		VALUES ($1, 1, 1, $2)
		ON CONFLICT (workspace_id) DO UPDATE SET settings = EXCLUDED.settings
	`, wsUUID, settingsJSON)
	if err != nil {
		t.Fatalf("update workspace governance config: %v", err)
	}

	// Create Jev Client on testHandler pointing to mockJevServer
	jevClient, err := jev.NewClient(jev.Options{
		APIKey:  "test-jev-key",
		BaseURL: mockJevServer.URL,
	})
	if err != nil {
		t.Fatalf("new jev client: %v", err)
	}
	origJev := testHandler.Jev
	testHandler.Jev = jevClient
	defer func() { testHandler.Jev = origJev }()

	// 5. Test Fast-Path Routing (confidence 0.95 >= 0.90)
	issueID := dbid.NewV7()
	_, err = testPool.Exec(ctx, `
		INSERT INTO issue (id, workspace_id, title, description, status, priority, assignee_type, assignee_id, creator_type, creator_id, revision, number)
		VALUES ($1, $2, 'Implement fast go routing', 'Needs Go backend specialist', 'backlog', 'high', 'squad', $3, 'member', $4, 1, 1001)
	`, issueID, wsUUID, squadID, testUserID)
	if err != nil {
		t.Fatalf("insert issue: %v", err)
	}
	defer testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, issueID)

	issue, err := testHandler.Queries.GetIssue(ctx, issueID)
	if err != nil {
		t.Fatalf("load issue: %v", err)
	}
	squad, err := testHandler.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: squadID, WorkspaceID: wsUUID})
	if err != nil {
		t.Fatalf("load squad: %v", err)
	}

	routed, advisory := testHandler.tryJevSquadRouting(ctx, issue, squad, "member", testUserID, testUserID)
	if !routed {
		t.Fatalf("expected fast-path routing = true, got false. Advisory: %s", advisory)
	}
	if advisory != "" {
		t.Errorf("expected empty advisory on success, got: %s", advisory)
	}

	// Verify issue status moved from backlog to in_progress
	updatedIssue, err := testHandler.Queries.GetIssue(ctx, issueID)
	if err != nil {
		t.Fatalf("reload issue: %v", err)
	}
	if updatedIssue.Status != "in_progress" {
		t.Errorf("issue status = %q, want in_progress", updatedIssue.Status)
	}

	// Verify announcement comment exists with type='jev_route'
	var commentType, commentContent string
	err = testPool.QueryRow(ctx, `
		SELECT type, content FROM comment WHERE issue_id = $1 AND type = 'jev_route' LIMIT 1
	`, issueID).Scan(&commentType, &commentContent)
	if err != nil {
		t.Fatalf("load jev_route comment: %v", err)
	}
	if !strings.Contains(commentContent, "⚡ Jev routed to squad-worker-sol (Tier S)") {
		t.Errorf("comment content unexpected: %s", commentContent)
	}
	if strings.Contains(commentContent, "mention://") {
		t.Errorf("comment content must NOT contain mention links: %s", commentContent)
	}

	// Verify squad activity was recorded
	var action, detailsStr string
	err = testPool.QueryRow(ctx, `
		SELECT action, details::text FROM activity_log WHERE issue_id = $1 AND action = 'squad_leader_evaluated' LIMIT 1
	`, issueID).Scan(&action, &detailsStr)
	if err != nil {
		t.Fatalf("load squad activity: %v", err)
	}
	if !strings.Contains(detailsStr, `"via":"jev"`) && !strings.Contains(detailsStr, `"via": "jev"`) {
		t.Errorf("activity details missing via:jev: %s", detailsStr)
	}

	// Verify member task was enqueued (NOT leader task)
	var memberTaskCount int
	_ = testPool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2
	`, issueID, memberAgentID).Scan(&memberTaskCount)
	if memberTaskCount == 0 {
		t.Errorf("expected worker task enqueued for member %s, got 0", util.UUIDToString(memberAgentID))
	}

	// 6. Test Fallback Routing (low confidence 0.70 < 0.90)
	returnConfidence = 0.70
	fallbackIssueID := dbid.NewV7()
	_, err = testPool.Exec(ctx, `
		INSERT INTO issue (id, workspace_id, title, description, status, priority, assignee_type, assignee_id, creator_type, creator_id, revision, number)
		VALUES ($1, $2, 'Ambiguous triage task', 'Not sure who should do this', 'backlog', 'high', 'squad', $3, 'member', $4, 1, 1002)
	`, fallbackIssueID, wsUUID, squadID, testUserID)
	if err != nil {
		t.Fatalf("insert fallback issue: %v", err)
	}
	defer testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, fallbackIssueID)

	fbIssue, _ := testHandler.Queries.GetIssue(ctx, fallbackIssueID)
	fbRouted, fbAdvisory := testHandler.tryJevSquadRouting(ctx, fbIssue, squad, "member", testUserID, testUserID)
	if fbRouted {
		t.Fatalf("expected fallback routing = false, got true")
	}
	if !strings.Contains(fbAdvisory, "### Jev Pre-Route Advisory") {
		t.Errorf("fallback advisory missing header: %s", fbAdvisory)
	}
	if !strings.Contains(fbAdvisory, "Confidence 0.70 is below 0.90 threshold") {
		t.Errorf("fallback advisory missing confidence reason: %s", fbAdvisory)
	}

	// Verify issue status remains backlog
	fbIssueAfter, _ := testHandler.Queries.GetIssue(ctx, fallbackIssueID)
	if fbIssueAfter.Status != "backlog" {
		t.Errorf("fallback issue status = %q, want backlog", fbIssueAfter.Status)
	}

	// 7. Test Concurrent Revision Update (concurrency conflict detected by FOR NO KEY UPDATE)
	returnConfidence = 0.95
	concurrencyIssueID := dbid.NewV7()
	_, err = testPool.Exec(ctx, `
		INSERT INTO issue (id, workspace_id, title, description, status, priority, assignee_type, assignee_id, creator_type, creator_id, revision, number)
		VALUES ($1, $2, 'Concurrent routing task', 'Check revision locking', 'backlog', 'high', 'squad', $3, 'member', $4, 1, 1003)
	`, concurrencyIssueID, wsUUID, squadID, testUserID)
	if err != nil {
		t.Fatalf("insert concurrency issue: %v", err)
	}
	defer testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, concurrencyIssueID)

	concIssue, _ := testHandler.Queries.GetIssue(ctx, concurrencyIssueID)
	// Concurrently bump issue revision in DB
	_, err = testPool.Exec(ctx, `UPDATE issue SET revision = revision + 1 WHERE id = $1`, concurrencyIssueID)
	if err != nil {
		t.Fatalf("concurrent update issue revision: %v", err)
	}
	// Calling with the stale issue struct (revision 1 vs DB revision 2) should detect the conflict and abort safely
	staleRouted, staleAdvisory := testHandler.tryJevSquadRouting(ctx, concIssue, squad, "member", testUserID, testUserID)
	if staleRouted {
		t.Fatalf("expected concurrent revision conflict routing = false, got true")
	}
	if !strings.Contains(staleAdvisory, "Issue revision changed during evaluation") {
		t.Errorf("expected revision drift advisory, got: %s", staleAdvisory)
	}
}

func TestParseSquadTierListEdgeCases(t *testing.T) {
	solID := dbid.NewV7()
	opusID := dbid.NewV7()

	candidates := []db.Agent{
		{ID: solID, Name: "sol"},
		{ID: opusID, Name: "x99-claude-opus"},
	}

	// Case 1: "keep a tier list" should default to C, not match A from "a"
	txt1 := "Instructions: keep a tier list updated regularly."
	tiers1 := parseSquadTierList(txt1, candidates)
	if got := tiers1[util.UUIDToString(solID)]; got != "C" {
		t.Errorf("case 1 sol tier = %q, want C", got)
	}
	if got := tiers1[util.UUIDToString(opusID)]; got != "C" {
		t.Errorf("case 1 opus tier = %q, want C", got)
	}

	// Case 2: "console" substring should not match agent "sol"
	txt2 := "Logs are printed to the console output."
	tiers2 := parseSquadTierList(txt2, candidates)
	if got := tiers2[util.UUIDToString(solID)]; got != "C" {
		t.Errorf("case 2 sol tier = %q, want C (no substring match inside console)", got)
	}

	// Case 3: Multiple candidates on one line: "x99-claude-opus is Tier S, sol is Tier D"
	txt3 := "x99-claude-opus is Tier S, sol is Tier D"
	tiers3 := parseSquadTierList(txt3, candidates)
	if got := tiers3[util.UUIDToString(opusID)]; got != "S" {
		t.Errorf("case 3 opus tier = %q, want S", got)
	}
	if got := tiers3[util.UUIDToString(solID)]; got != "D" {
		t.Errorf("case 3 sol tier = %q, want D", got)
	}

	// Case 4: Lowercase tier letter in table: "| a |" should default to C
	txt4 := `
| Member | Tier |
| sol | a |
`
	tiers4 := parseSquadTierList(txt4, candidates)
	if got := tiers4[util.UUIDToString(solID)]; got != "C" {
		t.Errorf("case 4 sol tier = %q, want C (lowercase tier letter rejected)", got)
	}
}
