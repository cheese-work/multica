package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/governance"
	"github.com/multica-ai/multica/server/internal/governance/credential"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/featureflag"
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
	squadOverLimit := strings.Repeat("squad ", 2000) // ~12000 chars, ~4000 tokens
	if jev.EstimateInstructionTokens(squadOverLimit) <= 3000 {
		t.Fatalf("expected squadOverLimit to exceed 3000 tokens, got %d", jev.EstimateInstructionTokens(squadOverLimit))
	}

	squadUnderLimit := "Short squad instructions"
	if jev.EstimateInstructionTokens(squadUnderLimit) > 3000 {
		t.Fatalf("expected squadUnderLimit under 3000 tokens, got %d", jev.EstimateInstructionTokens(squadUnderLimit))
	}

	// 4000 tokens limit for agent = ~12000 chars at 3.0 chars/token
	agentOverLimit := strings.Repeat("agent ", 3000) // ~18000 chars, ~6000 tokens
	if jev.EstimateInstructionTokens(agentOverLimit) <= 4000 {
		t.Fatalf("expected agentOverLimit to exceed 4000 tokens, got %d", jev.EstimateInstructionTokens(agentOverLimit))
	}

	agentUnderLimit := "Short agent instructions"
	if jev.EstimateInstructionTokens(agentUnderLimit) > 4000 {
		t.Fatalf("expected agentUnderLimit under 4000 tokens, got %d", jev.EstimateInstructionTokens(agentUnderLimit))
	}

	// Verify EstimateTokens uses byte length for state builder budgeting
	multiByteText := "xin chào việt nam 🎉"
	if jev.EstimateTokens(multiByteText) < jev.EstimateInstructionTokens(multiByteText) {
		t.Fatalf("expected byte-based EstimateTokens >= rune-based EstimateInstructionTokens for multibyte text")
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

type failingRow struct{}

func (f *failingRow) Scan(dest ...interface{}) error {
	return errors.New("simulated task enqueue db failure")
}

type failingDBTX struct{}

func (f *failingDBTX) Exec(ctx context.Context, sql string, arguments ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("simulated task enqueue db failure")
}

func (f *failingDBTX) Query(ctx context.Context, sql string, args ...interface{}) (pgx.Rows, error) {
	return nil, errors.New("simulated task enqueue db failure")
}

func (f *failingDBTX) QueryRow(ctx context.Context, sql string, args ...interface{}) pgx.Row {
	return &failingRow{}
}

func TestTryJevSquadRouting_TodoStatusAndInProgress(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}

	ctx := context.Background()
	wsUUID, _ := util.ParseUUID(testWorkspaceID)
	rtUUID, _ := util.ParseUUID(testRuntimeID)
	userUUID, _ := util.ParseUUID(testUserID)

	leaderAgentID := dbid.NewV7()
	memberAgentID := dbid.NewV7()
	_, err := testPool.Exec(ctx, `
		INSERT INTO agent (id, workspace_id, runtime_id, owner_id, name, status, runtime_mode)
		VALUES ($1, $2, $3, $4, 'todo-test-leader', 'idle', 'cloud'),
		       ($5, $2, $3, $4, 'todo-test-worker', 'idle', 'cloud')
	`, leaderAgentID, wsUUID, rtUUID, userUUID, memberAgentID)
	if err != nil {
		t.Fatalf("insert test agents: %v", err)
	}
	defer testPool.Exec(ctx, `DELETE FROM agent WHERE id IN ($1, $2)`, leaderAgentID, memberAgentID)

	squadID := dbid.NewV7()
	_, err = testPool.Exec(ctx, `
		INSERT INTO squad (id, workspace_id, name, description, instructions, leader_id, creator_id)
		VALUES ($1, $2, 'Todo Test Squad', '', 'Tier S: todo-test-worker', $3, $4)
	`, squadID, wsUUID, leaderAgentID, userUUID)
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

	mockJevServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := jev.Response{
			Model: "jev-1.13.0",
			Answers: map[string]jev.Answer{
				"route_squad_member": {
					Type:       jev.TypeChoice,
					Choice:     util.UUIDToString(memberAgentID),
					Confidence: 0.95,
					Probabilities: map[string]float64{
						util.UUIDToString(memberAgentID): 0.95,
						"escalate_to_leader":             0.05,
					},
				},
			},
			Usage: jev.Usage{InputTokens: 500, OutputTokens: 10},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockJevServer.Close()

	var minConf = 0.90
	settings := governance.WorkspaceSettings{
		JevSquadRoutingEnabled: true,
		JevAPIURL:              mockJevServer.URL,
		JevMinConfidence:       &minConf,
	}
	settingsJSON, _ := json.Marshal(settings)
	_, _ = testPool.Exec(ctx, `
		INSERT INTO governance_workspace_config (workspace_id, config_version, control_epoch, settings)
		VALUES ($1, 1, 1, $2)
		ON CONFLICT (workspace_id) DO UPDATE SET settings = EXCLUDED.settings
	`, wsUUID, settingsJSON)

	jevClient, _ := jev.NewClient(jev.Options{
		APIKey:  "test-jev-key",
		BaseURL: mockJevServer.URL,
	})
	origJev := testHandler.Jev
	testHandler.Jev = jevClient
	defer func() { testHandler.Jev = origJev }()

	squad, _ := testHandler.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: squadID, WorkspaceID: wsUUID})

	// 1. Test todo -> in_progress transition
	todoIssueID := dbid.NewV7()
	_, _ = testPool.Exec(ctx, `
		INSERT INTO issue (id, workspace_id, title, description, status, priority, assignee_type, assignee_id, creator_type, creator_id, revision, number)
		VALUES ($1, $2, 'Todo task', 'Promoted from backlog to todo', 'todo', 'high', 'squad', $3, 'member', $4, 1, 2001)
	`, todoIssueID, wsUUID, squadID, testUserID)
	defer testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, todoIssueID)

	todoIssue, _ := testHandler.Queries.GetIssue(ctx, todoIssueID)
	routed, _ := testHandler.tryJevSquadRouting(ctx, todoIssue, squad, "member", testUserID, testUserID)
	if !routed {
		t.Fatalf("expected routing from todo to succeed")
	}
	afterTodoIssue, _ := testHandler.Queries.GetIssue(ctx, todoIssueID)
	if afterTodoIssue.Status != "in_progress" {
		t.Errorf("status after routing = %q, want in_progress", afterTodoIssue.Status)
	}

	// 2. Test in_progress status remains in_progress (does NOT jump to in_review)
	inProgIssueID := dbid.NewV7()
	_, _ = testPool.Exec(ctx, `
		INSERT INTO issue (id, workspace_id, title, description, status, priority, assignee_type, assignee_id, creator_type, creator_id, revision, number)
		VALUES ($1, $2, 'In progress task', 'Already in progress', 'in_progress', 'high', 'squad', $3, 'member', $4, 1, 2002)
	`, inProgIssueID, wsUUID, squadID, testUserID)
	defer testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, inProgIssueID)

	inProgIssue, _ := testHandler.Queries.GetIssue(ctx, inProgIssueID)
	routed2, _ := testHandler.tryJevSquadRouting(ctx, inProgIssue, squad, "member", testUserID, testUserID)
	if !routed2 {
		t.Fatalf("expected routing from in_progress to succeed")
	}
	afterInProgIssue, _ := testHandler.Queries.GetIssue(ctx, inProgIssueID)
	if afterInProgIssue.Status != "in_progress" {
		t.Errorf("status after routing = %q, want in_progress (must not jump to in_review)", afterInProgIssue.Status)
	}
}

func TestTryJevSquadRouting_EnqueueFailureAdvisory(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}

	ctx := context.Background()
	wsUUID, _ := util.ParseUUID(testWorkspaceID)
	rtUUID, _ := util.ParseUUID(testRuntimeID)
	userUUID, _ := util.ParseUUID(testUserID)

	leaderAgentID := dbid.NewV7()
	memberAgentID := dbid.NewV7()
	_, _ = testPool.Exec(ctx, `
		INSERT INTO agent (id, workspace_id, runtime_id, owner_id, name, status, runtime_mode)
		VALUES ($1, $2, $3, $4, 'fail-test-leader', 'idle', 'cloud'),
		       ($5, $2, $3, $4, 'fail-test-worker', 'idle', 'cloud')
	`, leaderAgentID, wsUUID, rtUUID, userUUID, memberAgentID)
	defer testPool.Exec(ctx, `DELETE FROM agent WHERE id IN ($1, $2)`, leaderAgentID, memberAgentID)

	squadID := dbid.NewV7()
	_, _ = testPool.Exec(ctx, `
		INSERT INTO squad (id, workspace_id, name, description, instructions, leader_id, creator_id)
		VALUES ($1, $2, 'Fail Test Squad', '', 'Tier S: fail-test-worker', $3, $4)
	`, squadID, wsUUID, leaderAgentID, userUUID)
	defer testPool.Exec(ctx, `DELETE FROM squad WHERE id = $1`, squadID)

	_, _ = testPool.Exec(ctx, `
		INSERT INTO squad_member (id, squad_id, member_type, member_id, role)
		VALUES ($1, $2, 'agent', $3, 'worker')
	`, dbid.NewV7(), squadID, memberAgentID)

	mockJevServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := jev.Response{
			Model: "jev-1.13.0",
			Answers: map[string]jev.Answer{
				"route_squad_member": {
					Type:       jev.TypeChoice,
					Choice:     util.UUIDToString(memberAgentID),
					Confidence: 0.95,
					Probabilities: map[string]float64{
						util.UUIDToString(memberAgentID): 0.95,
						"escalate_to_leader":             0.05,
					},
				},
			},
			Usage: jev.Usage{InputTokens: 500, OutputTokens: 10},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockJevServer.Close()

	var minConf = 0.90
	settings := governance.WorkspaceSettings{
		JevSquadRoutingEnabled: true,
		JevAPIURL:              mockJevServer.URL,
		JevMinConfidence:       &minConf,
	}
	settingsJSON, _ := json.Marshal(settings)
	_, _ = testPool.Exec(ctx, `
		INSERT INTO governance_workspace_config (workspace_id, config_version, control_epoch, settings)
		VALUES ($1, 1, 1, $2)
		ON CONFLICT (workspace_id) DO UPDATE SET settings = EXCLUDED.settings
	`, wsUUID, settingsJSON)

	jevClient, _ := jev.NewClient(jev.Options{
		APIKey:  "test-jev-key",
		BaseURL: mockJevServer.URL,
	})
	origJev := testHandler.Jev
	testHandler.Jev = jevClient
	defer func() { testHandler.Jev = origJev }()

	squad, _ := testHandler.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: squadID, WorkspaceID: wsUUID})

	// Temporarily simulate task enqueue db failure
	origQueries := testHandler.TaskService.Queries
	testHandler.TaskService.Queries = db.New(&failingDBTX{})
	defer func() { testHandler.TaskService.Queries = origQueries }()

	issueID := dbid.NewV7()
	_, _ = testPool.Exec(ctx, `
		INSERT INTO issue (id, workspace_id, title, description, status, priority, assignee_type, assignee_id, creator_type, creator_id, revision, number)
		VALUES ($1, $2, 'Task with enqueue failure', 'Tests advisory formatting on failure', 'backlog', 'high', 'squad', $3, 'member', $4, 1, 3001)
	`, issueID, wsUUID, squadID, testUserID)
	defer testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, issueID)

	issue, _ := testHandler.Queries.GetIssue(ctx, issueID)
	routed, advisory := testHandler.tryJevSquadRouting(ctx, issue, squad, "member", testUserID, testUserID)
	if routed {
		t.Fatalf("expected routing = false when task enqueue fails")
	}
	if !strings.Contains(advisory, "Prior State Changes: status -> in_progress (retained), jev_route comment (retained), outcome=action activity (retained)") {
		t.Errorf("advisory missing retained status and events: %s", advisory)
	}
	if !strings.Contains(advisory, "simulated task enqueue db failure") {
		t.Errorf("advisory missing enqueue failure reason: %s", advisory)
	}
}

func TestParseSquadTierList_EdgeCases(t *testing.T) {
	specialistSolID := dbid.NewV7()
	agentLunaID := dbid.NewV7()
	memberAlphaID := dbid.NewV7()
	memberBetaID := dbid.NewV7()

	candidates := []db.Agent{
		{ID: specialistSolID, Name: "specialist-sol"},
		{ID: agentLunaID, Name: "agent-luna"},
		{ID: memberAlphaID, Name: "member-alpha"},
		{ID: memberBetaID, Name: "member-beta"},
	}

	// 1. Table with separator row and member names containing "specialist", "agent" in data rows
	markdownTable := `
# Team Roster
| Member | Tier | Notes |
|:---|:---:|---|
| specialist-sol | S | Lead backend |
| agent-luna | A | QA |
`
	tiers := parseSquadTierList(markdownTable, candidates)
	if got := tiers[util.UUIDToString(specialistSolID)]; got != "S" {
		t.Errorf("specialist-sol tier = %q, want S", got)
	}
	if got := tiers[util.UUIDToString(agentLunaID)]; got != "A" {
		t.Errorf("agent-luna tier = %q, want A", got)
	}

	// 2. Candidate clause isolation: member-alpha has no tier in its clause, member-beta does
	inlineText := `
- member-alpha: frontend developer; member-beta: Tier S specialist
`
	tiersInline := parseSquadTierList(inlineText, candidates)
	if got := tiersInline[util.UUIDToString(memberAlphaID)]; got != "C" {
		t.Errorf("member-alpha tier = %q, want default C (must not steal member-beta's Tier S)", got)
	}
	if got := tiersInline[util.UUIDToString(memberBetaID)]; got != "S" {
		t.Errorf("member-beta tier = %q, want S", got)
	}
}

func TestTryJevSquadRouting_CommentWindowing(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}

	ctx := context.Background()
	wsUUID, _ := util.ParseUUID(testWorkspaceID)
	rtUUID, _ := util.ParseUUID(testRuntimeID)
	userUUID, _ := util.ParseUUID(testUserID)

	leaderAgentID := dbid.NewV7()
	memberAgentID := dbid.NewV7()
	_, err := testPool.Exec(ctx, `
		INSERT INTO agent (id, workspace_id, runtime_id, owner_id, name, status, runtime_mode)
		VALUES ($1, $2, $3, $4, 'window-test-leader', 'idle', 'cloud'),
		       ($5, $2, $3, $4, 'window-test-worker', 'idle', 'cloud')
	`, leaderAgentID, wsUUID, rtUUID, userUUID, memberAgentID)
	if err != nil {
		t.Fatalf("insert test agents: %v", err)
	}
	defer testPool.Exec(ctx, `DELETE FROM agent WHERE id IN ($1, $2)`, leaderAgentID, memberAgentID)

	squadID := dbid.NewV7()
	_, err = testPool.Exec(ctx, `
		INSERT INTO squad (id, workspace_id, name, description, instructions, leader_id, creator_id)
		VALUES ($1, $2, 'Window Test Squad', '', 'Tier S: window-test-worker', $3, $4)
	`, squadID, wsUUID, leaderAgentID, userUUID)
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

	var capturedComments []map[string]any
	mockJevServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			if stateMap, ok := body["state"].(map[string]any); ok {
				if commentsList, ok := stateMap["comments"].([]any); ok {
					for _, item := range commentsList {
						if cMap, ok := item.(map[string]any); ok {
							capturedComments = append(capturedComments, cMap)
						}
					}
				}
			}
		}
		resp := jev.Response{
			Model: "jev-1.13.0",
			Answers: map[string]jev.Answer{
				"route_squad_member": {
					Type:       jev.TypeChoice,
					Choice:     util.UUIDToString(memberAgentID),
					Confidence: 0.95,
					Probabilities: map[string]float64{
						util.UUIDToString(memberAgentID): 0.95,
						"escalate_to_leader":             0.05,
					},
				},
			},
			Usage: jev.Usage{InputTokens: 500, OutputTokens: 10},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockJevServer.Close()

	var minConf = 0.90
	settings := governance.WorkspaceSettings{
		JevSquadRoutingEnabled: true,
		JevAPIURL:              mockJevServer.URL,
		JevMinConfidence:       &minConf,
	}
	settingsJSON, _ := json.Marshal(settings)
	_, _ = testPool.Exec(ctx, `
		INSERT INTO governance_workspace_config (workspace_id, config_version, control_epoch, settings)
		VALUES ($1, 1, 1, $2)
		ON CONFLICT (workspace_id) DO UPDATE SET settings = EXCLUDED.settings
	`, wsUUID, settingsJSON)

	jevClient, _ := jev.NewClient(jev.Options{
		APIKey:  "test-jev-key",
		BaseURL: mockJevServer.URL,
	})
	origJev := testHandler.Jev
	testHandler.Jev = jevClient
	defer func() { testHandler.Jev = origJev }()

	squad, _ := testHandler.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: squadID, WorkspaceID: wsUUID})

	issueID := dbid.NewV7()
	_, _ = testPool.Exec(ctx, `
		INSERT INTO issue (id, workspace_id, title, description, status, priority, assignee_type, assignee_id, creator_type, creator_id, revision, number)
		VALUES ($1, $2, 'Windowing Issue', 'Testing comment windowing newest first', 'backlog', 'high', 'squad', $3, 'member', $4, 1, 4001)
	`, issueID, wsUUID, squadID, testUserID)
	defer testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, issueID)

	// Insert 20 comments with created_at strictly ascending.
	// Each comment has ~4500 characters (~1500 tokens).
	// Total 20 * 1500 = 30000 tokens > 14000 limit.
	// Comments 1..19 are system/bot comments.
	// Comment 20 is the latest human (member) comment.
	for i := 1; i <= 20; i++ {
		commentID := dbid.NewV7()
		authorType := "system"
		if i == 20 {
			authorType = "member"
		}
		content := fmt.Sprintf("Comment %02d: %s", i, strings.Repeat("detailed log payload ", 210))
		_, err := testPool.Exec(ctx, `
			INSERT INTO comment (id, workspace_id, issue_id, author_type, author_id, content, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, NOW() + ($7 * interval '1 second'), NOW() + ($7 * interval '1 second'))
		`, commentID, wsUUID, issueID, authorType, testUserID, content, i)
		if err != nil {
			t.Fatalf("insert comment %d: %v", i, err)
		}
	}

	issue, _ := testHandler.Queries.GetIssue(ctx, issueID)
	routed, adv := testHandler.tryJevSquadRouting(ctx, issue, squad, "member", testUserID, testUserID)
	if !routed {
		t.Fatalf("tryJevSquadRouting failed: %s", adv)
	}

	if len(capturedComments) == 0 {
		t.Fatalf("no comments were sent to Jev")
	}

	// Verify latest comment (comment 20, human) is present
	foundLatestHuman := false
	for _, c := range capturedComments {
		content, _ := c["content"].(string)
		if strings.HasPrefix(content, "Comment 20:") {
			foundLatestHuman = true
			if len(content) < 4000 {
				t.Errorf("latest human comment was unexpectedly truncated: len=%d", len(content))
			}
		}
	}
	if !foundLatestHuman {
		t.Fatalf("latest human comment (Comment 20) was dropped by comment windowing")
	}

	// Verify oldest comment (Comment 01) was dropped because 14,000 token limit was reached
	firstCommentContent, _ := capturedComments[0]["content"].(string)
	if strings.HasPrefix(firstCommentContent, "Comment 01:") {
		t.Fatalf("oldest comment (Comment 01) was unexpectedly included; newest comments should be prioritized")
	}

	// Verify chronological order in captured comments
	lastSeen := -1
	for _, c := range capturedComments {
		content, _ := c["content"].(string)
		var num int
		if n, _ := fmt.Sscanf(content, "Comment %d:", &num); n == 1 {
			if num <= lastSeen {
				t.Errorf("comments not in chronological order: saw %d after %d", num, lastSeen)
			}
			lastSeen = num
		}
	}
}

func TestTryJevSquadRouting_WorkerCanInvokeAgentDenial(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}

	ctx := context.Background()
	wsUUID, _ := util.ParseUUID(testWorkspaceID)
	rtUUID, _ := util.ParseUUID(testRuntimeID)
	userUUID, _ := util.ParseUUID(testUserID)

	var otherUserID string
	if err := testPool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id`, "Other User", fmt.Sprintf("other-%s@multica.test", dbid.NewV7().String()[:8])).Scan(&otherUserID); err != nil {
		t.Fatalf("create other user: %v", err)
	}
	defer testPool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, otherUserID)

	leaderAgentID := dbid.NewV7()
	privateWorkerID := dbid.NewV7()
	_, err := testPool.Exec(ctx, `
		INSERT INTO agent (id, workspace_id, runtime_id, owner_id, name, status, runtime_mode, permission_mode)
		VALUES ($1, $2, $3, $4, 'private-test-leader', 'idle', 'cloud', 'public_to'),
		       ($5, $2, $3, $6, 'private-test-worker', 'idle', 'cloud', 'private')
	`, leaderAgentID, wsUUID, rtUUID, userUUID, privateWorkerID, otherUserID)
	if err != nil {
		t.Fatalf("insert test agents: %v", err)
	}
	defer testPool.Exec(ctx, `DELETE FROM agent WHERE id IN ($1, $2)`, leaderAgentID, privateWorkerID)

	squadID := dbid.NewV7()
	_, err = testPool.Exec(ctx, `
		INSERT INTO squad (id, workspace_id, name, description, instructions, leader_id, creator_id)
		VALUES ($1, $2, 'Private Worker Squad', '', 'Tier S: private-test-worker', $3, $4)
	`, squadID, wsUUID, leaderAgentID, userUUID)
	if err != nil {
		t.Fatalf("insert squad: %v", err)
	}
	defer testPool.Exec(ctx, `DELETE FROM squad WHERE id = $1`, squadID)

	_, err = testPool.Exec(ctx, `
		INSERT INTO squad_member (id, squad_id, member_type, member_id, role)
		VALUES ($1, $2, 'agent', $3, 'worker')
	`, dbid.NewV7(), squadID, privateWorkerID)
	if err != nil {
		t.Fatalf("insert squad member: %v", err)
	}

	mockJevServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := jev.Response{
			Model: "jev-1.13.0",
			Answers: map[string]jev.Answer{
				"route_squad_member": {
					Type:       jev.TypeChoice,
					Choice:     util.UUIDToString(privateWorkerID),
					Confidence: 0.95,
					Probabilities: map[string]float64{
						util.UUIDToString(privateWorkerID): 0.95,
						"escalate_to_leader":               0.05,
					},
				},
			},
			Usage: jev.Usage{InputTokens: 500, OutputTokens: 10},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockJevServer.Close()

	var minConf = 0.90
	settings := governance.WorkspaceSettings{
		JevSquadRoutingEnabled: true,
		JevAPIURL:              mockJevServer.URL,
		JevMinConfidence:       &minConf,
	}
	settingsJSON, _ := json.Marshal(settings)
	_, _ = testPool.Exec(ctx, `
		INSERT INTO governance_workspace_config (workspace_id, config_version, control_epoch, settings)
		VALUES ($1, 1, 1, $2)
		ON CONFLICT (workspace_id) DO UPDATE SET settings = EXCLUDED.settings
	`, wsUUID, settingsJSON)

	jevClient, _ := jev.NewClient(jev.Options{
		APIKey:  "test-jev-key",
		BaseURL: mockJevServer.URL,
	})
	origJev := testHandler.Jev
	testHandler.Jev = jevClient
	defer func() { testHandler.Jev = origJev }()

	squad, _ := testHandler.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: squadID, WorkspaceID: wsUUID})

	issueID := dbid.NewV7()
	_, _ = testPool.Exec(ctx, `
		INSERT INTO issue (id, workspace_id, title, description, status, priority, assignee_type, assignee_id, creator_type, creator_id, revision, number)
		VALUES ($1, $2, 'Invoke Denial Issue', 'Tests canInvokeAgent failure', 'backlog', 'high', 'squad', $3, 'member', $4, 1, 4002)
	`, issueID, wsUUID, squadID, testUserID)
	defer testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, issueID)

	issue, _ := testHandler.Queries.GetIssue(ctx, issueID)
	// Calling with testUserID who is not owner of privateWorkerID
	routed, adv := testHandler.tryJevSquadRouting(ctx, issue, squad, "member", testUserID, testUserID)
	if routed {
		t.Fatalf("expected routing = false when actor cannot invoke private worker")
	}
	if !strings.Contains(adv, "Actor not permitted to invoke selected specialist agent") {
		t.Errorf("advisory missing invoke denial reason: %s", adv)
	}
}

func TestTryJevSquadRouting_WorkerPendingDedup(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}

	ctx := context.Background()
	wsUUID, _ := util.ParseUUID(testWorkspaceID)
	rtUUID, _ := util.ParseUUID(testRuntimeID)
	userUUID, _ := util.ParseUUID(testUserID)

	leaderAgentID := dbid.NewV7()
	memberAgentID := dbid.NewV7()
	_, err := testPool.Exec(ctx, `
		INSERT INTO agent (id, workspace_id, runtime_id, owner_id, name, status, runtime_mode)
		VALUES ($1, $2, $3, $4, 'dedup-test-leader', 'idle', 'cloud'),
		       ($5, $2, $3, $4, 'dedup-test-worker', 'idle', 'cloud')
	`, leaderAgentID, wsUUID, rtUUID, userUUID, memberAgentID)
	if err != nil {
		t.Fatalf("insert test agents: %v", err)
	}
	defer testPool.Exec(ctx, `DELETE FROM agent WHERE id IN ($1, $2)`, leaderAgentID, memberAgentID)

	squadID := dbid.NewV7()
	_, err = testPool.Exec(ctx, `
		INSERT INTO squad (id, workspace_id, name, description, instructions, leader_id, creator_id)
		VALUES ($1, $2, 'Dedup Test Squad', '', 'Tier S: dedup-test-worker', $3, $4)
	`, squadID, wsUUID, leaderAgentID, userUUID)
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

	mockJevServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := jev.Response{
			Model: "jev-1.13.0",
			Answers: map[string]jev.Answer{
				"route_squad_member": {
					Type:       jev.TypeChoice,
					Choice:     util.UUIDToString(memberAgentID),
					Confidence: 0.95,
					Probabilities: map[string]float64{
						util.UUIDToString(memberAgentID): 0.95,
						"escalate_to_leader":             0.05,
					},
				},
			},
			Usage: jev.Usage{InputTokens: 500, OutputTokens: 10},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockJevServer.Close()

	var minConf = 0.90
	settings := governance.WorkspaceSettings{
		JevSquadRoutingEnabled: true,
		JevAPIURL:              mockJevServer.URL,
		JevMinConfidence:       &minConf,
	}
	settingsJSON, _ := json.Marshal(settings)
	_, _ = testPool.Exec(ctx, `
		INSERT INTO governance_workspace_config (workspace_id, config_version, control_epoch, settings)
		VALUES ($1, 1, 1, $2)
		ON CONFLICT (workspace_id) DO UPDATE SET settings = EXCLUDED.settings
	`, wsUUID, settingsJSON)

	jevClient, _ := jev.NewClient(jev.Options{
		APIKey:  "test-jev-key",
		BaseURL: mockJevServer.URL,
	})
	origJev := testHandler.Jev
	testHandler.Jev = jevClient
	defer func() { testHandler.Jev = origJev }()

	squad, _ := testHandler.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: squadID, WorkspaceID: wsUUID})

	issueID := dbid.NewV7()
	_, _ = testPool.Exec(ctx, `
		INSERT INTO issue (id, workspace_id, title, description, status, priority, assignee_type, assignee_id, creator_type, creator_id, revision, number)
		VALUES ($1, $2, 'Pending Dedup Issue', 'Tests pending task dedup', 'backlog', 'high', 'squad', $3, 'member', $4, 1, 4003)
	`, issueID, wsUUID, squadID, testUserID)
	defer testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, issueID)

	// Insert an existing queued task in agent_task_queue for memberAgentID on this issue
	taskID := dbid.NewV7()
	_, err = testPool.Exec(ctx, `
		INSERT INTO agent_task_queue (id, agent_id, runtime_id, issue_id, status, priority, context)
		VALUES ($1, $2, $3, $4, 'queued', 0, '{}'::jsonb)
	`, taskID, memberAgentID, rtUUID, issueID)
	if err != nil {
		t.Fatalf("insert pending task: %v", err)
	}
	defer testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE id = $1`, taskID)

	issue, _ := testHandler.Queries.GetIssue(ctx, issueID)
	routed, adv := testHandler.tryJevSquadRouting(ctx, issue, squad, "member", testUserID, testUserID)
	if routed {
		t.Fatalf("expected routing = false when specialist already has pending task")
	}
	if !strings.Contains(adv, "already has a pending task for this issue") {
		t.Errorf("advisory missing pending task dedup reason: %s", adv)
	}
}

func TestTryJevSquadRouting_SpendGateDenial(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}

	ctx := context.Background()
	wsUUID, _ := util.ParseUUID(testWorkspaceID)
	rtUUID, _ := util.ParseUUID(testRuntimeID)
	userUUID, _ := util.ParseUUID(testUserID)

	leaderAgentID := dbid.NewV7()
	memberAgentID := dbid.NewV7()
	_, err := testPool.Exec(ctx, `
		INSERT INTO agent (id, workspace_id, runtime_id, owner_id, name, status, runtime_mode)
		VALUES ($1, $2, $3, $4, 'gate-test-leader', 'idle', 'cloud'),
		       ($5, $2, $3, $4, 'gate-test-worker', 'idle', 'cloud')
	`, leaderAgentID, wsUUID, rtUUID, userUUID, memberAgentID)
	if err != nil {
		t.Fatalf("insert test agents: %v", err)
	}
	defer testPool.Exec(ctx, `DELETE FROM agent WHERE id IN ($1, $2)`, leaderAgentID, memberAgentID)

	squadID := dbid.NewV7()
	_, err = testPool.Exec(ctx, `
		INSERT INTO squad (id, workspace_id, name, description, instructions, leader_id, creator_id)
		VALUES ($1, $2, 'Gate Test Squad', '', 'Tier S: gate-test-worker', $3, $4)
	`, squadID, wsUUID, leaderAgentID, userUUID)
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

	mockJevServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("network call unexpectedly reached mock server when gate is denied")
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer mockJevServer.Close()

	var minConf = 0.90
	settings := governance.WorkspaceSettings{
		JevSquadRoutingEnabled: true,
		JevAPIURL:              mockJevServer.URL,
		JevMinConfidence:       &minConf,
	}
	settingsJSON, _ := json.Marshal(settings)
	_, _ = testPool.Exec(ctx, `
		INSERT INTO governance_workspace_config (workspace_id, config_version, control_epoch, settings)
		VALUES ($1, 1, 1, $2)
		ON CONFLICT (workspace_id) DO UPDATE SET settings = EXCLUDED.settings
	`, wsUUID, settingsJSON)

	// Configure gate that explicitly denies (e.g. Rule{Default: false})
	sp := featureflag.NewStaticProvider()
	sp.LoadRules(map[string]featureflag.Rule{featureflags.Jev: {Default: false}})
	flagService := featureflag.NewService(sp)

	// Exercise per-workspace credential path (ResolveJevCredential -> jev.NewClient with Gate)
	ring, err := credential.NewKeyring(credential.Key{ID: "current", Material: bytes.Repeat([]byte{1}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := ring.Seal([]byte("test-jev-key"), credential.Binding{
		WorkspaceID:  uuidToString(wsUUID),
		CredentialID: "api-key",
		Purpose:      jevCredentialPurpose,
	})
	if err != nil {
		t.Fatal(err)
	}
	encodedEnv, _ := json.Marshal(envelope)
	_, err = testPool.Exec(ctx, `
		INSERT INTO governance_jev_credential (workspace_id, envelope, updated_by)
		VALUES ($1, $2, $3)
		ON CONFLICT (workspace_id) DO UPDATE SET envelope = EXCLUDED.envelope
	`, wsUUID, encodedEnv, userUUID)
	if err != nil {
		t.Fatal(err)
	}
	defer testPool.Exec(ctx, `DELETE FROM governance_jev_credential WHERE workspace_id = $1`, wsUUID)

	origJev := testHandler.Jev
	origCreds := testHandler.JevCredentials
	origFlags := testHandler.FeatureFlags
	testHandler.Jev = nil // explicitly nil to force per-workspace credential resolution
	testHandler.JevCredentials = ring
	testHandler.FeatureFlags = flagService
	defer func() {
		testHandler.Jev = origJev
		testHandler.JevCredentials = origCreds
		testHandler.FeatureFlags = origFlags
	}()

	squad, _ := testHandler.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: squadID, WorkspaceID: wsUUID})

	issueID := dbid.NewV7()
	_, _ = testPool.Exec(ctx, `
		INSERT INTO issue (id, workspace_id, title, description, status, priority, assignee_type, assignee_id, creator_type, creator_id, revision, number)
		VALUES ($1, $2, 'Gate Denial Issue', 'Tests spend gate denial before network', 'backlog', 'high', 'squad', $3, 'member', $4, 1, 4004)
	`, issueID, wsUUID, squadID, testUserID)
	defer testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, issueID)

	issue, _ := testHandler.Queries.GetIssue(ctx, issueID)
	routed, adv := testHandler.tryJevSquadRouting(ctx, issue, squad, "member", testUserID, testUserID)
	if routed {
		t.Fatalf("expected routing = false when JevProductionGate denies")
	}
	if !strings.Contains(adv, "Jev evaluation failed") {
		t.Errorf("advisory missing gate denial error: %s", adv)
	}
}

func TestTryJevSquadRouting_LatestHumanOutsideWindow(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}

	ctx := context.Background()
	wsUUID, _ := util.ParseUUID(testWorkspaceID)
	rtUUID, _ := util.ParseUUID(testRuntimeID)
	userUUID, _ := util.ParseUUID(testUserID)

	leaderAgentID := dbid.NewV7()
	memberAgentID := dbid.NewV7()
	_, err := testPool.Exec(ctx, `
		INSERT INTO agent (id, workspace_id, runtime_id, owner_id, name, status, runtime_mode)
		VALUES ($1, $2, $3, $4, 'window-leader', 'idle', 'cloud'),
		       ($5, $2, $3, $4, 'window-worker', 'idle', 'cloud')
	`, leaderAgentID, wsUUID, rtUUID, userUUID, memberAgentID)
	if err != nil {
		t.Fatalf("insert test agents: %v", err)
	}
	defer testPool.Exec(ctx, `DELETE FROM agent WHERE id IN ($1, $2)`, leaderAgentID, memberAgentID)

	squadID := dbid.NewV7()
	_, err = testPool.Exec(ctx, `
		INSERT INTO squad (id, workspace_id, name, description, instructions, leader_id, creator_id)
		VALUES ($1, $2, 'Window Squad', '', 'Tier S: window-worker', $3, $4)
	`, squadID, wsUUID, leaderAgentID, userUUID)
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

	var capturedLatestHuman map[string]any
	mockJevServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jev.Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		if st, ok := req.State.(map[string]any); ok {
			if lhc, ok := st["latest_human_comment"].(map[string]any); ok {
				capturedLatestHuman = lhc
			}
		}
		resp := jev.Response{
			Model: "jev-1.13.0",
			Answers: map[string]jev.Answer{
				"route_squad_member": {
					Type:       jev.TypeChoice,
					Choice:     util.UUIDToString(memberAgentID),
					Confidence: 0.95,
					Probabilities: map[string]float64{
						util.UUIDToString(memberAgentID): 0.95,
						"escalate_to_leader":             0.05,
					},
				},
			},
			Usage: jev.Usage{InputTokens: 500, OutputTokens: 10},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockJevServer.Close()

	var minConf = 0.90
	settings := governance.WorkspaceSettings{
		JevSquadRoutingEnabled: true,
		JevAPIURL:              mockJevServer.URL,
		JevMinConfidence:       &minConf,
	}
	settingsJSON, _ := json.Marshal(settings)
	_, _ = testPool.Exec(ctx, `
		INSERT INTO governance_workspace_config (workspace_id, config_version, control_epoch, settings)
		VALUES ($1, 1, 1, $2)
		ON CONFLICT (workspace_id) DO UPDATE SET settings = EXCLUDED.settings
	`, wsUUID, settingsJSON)

	jevClient, _ := jev.NewClient(jev.Options{
		APIKey:  "test-jev-key",
		BaseURL: mockJevServer.URL,
	})
	origJev := testHandler.Jev
	testHandler.Jev = jevClient
	defer func() { testHandler.Jev = origJev }()

	squad, _ := testHandler.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: squadID, WorkspaceID: wsUUID})

	issueID := dbid.NewV7()
	_, _ = testPool.Exec(ctx, `
		INSERT INTO issue (id, workspace_id, title, description, status, priority, assignee_type, assignee_id, creator_type, creator_id, revision, number)
		VALUES ($1, $2, 'Window Test Issue', 'Issue testing human outside 20 window', 'backlog', 'high', 'squad', $3, 'member', $4, 1, 4005)
	`, issueID, wsUUID, squadID, testUserID)
	defer testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, issueID)

	// Insert 1 older human comment
	humanCommentID := dbid.NewV7()
	humanText := "Crucial human instruction posted before 25 agent chatter comments"
	_, err = testPool.Exec(ctx, `
		INSERT INTO comment (id, workspace_id, issue_id, author_type, author_id, content, created_at, updated_at)
		VALUES ($1, $2, $3, 'member', $4, $5, NOW() - interval '1 hour', NOW() - interval '1 hour')
	`, humanCommentID, wsUUID, issueID, testUserID, humanText)
	if err != nil {
		t.Fatalf("insert human comment: %v", err)
	}

	// Insert 25 bot/agent comments
	for i := 1; i <= 25; i++ {
		_, err = testPool.Exec(ctx, `
			INSERT INTO comment (id, workspace_id, issue_id, author_type, author_id, content, created_at, updated_at)
			VALUES ($1, $2, $3, 'agent', $4, $5, NOW() - (interval '1 minute' * $6), NOW() - (interval '1 minute' * $6))
		`, dbid.NewV7(), wsUUID, issueID, leaderAgentID, fmt.Sprintf("Agent chatter log line %d", i), 26-i)
		if err != nil {
			t.Fatalf("insert agent comment %d: %v", i, err)
		}
	}

	issue, _ := testHandler.Queries.GetIssue(ctx, issueID)
	routed, adv := testHandler.tryJevSquadRouting(ctx, issue, squad, "member", testUserID, testUserID)
	if !routed {
		t.Fatalf("expected routing = true when older human comment exists outside 20 window, got false: %s", adv)
	}

	if capturedLatestHuman == nil {
		t.Fatalf("expected latest_human_comment to be captured in State, was nil")
	}
	if content, _ := capturedLatestHuman["content"].(string); content != humanText {
		t.Errorf("latest_human_comment content = %q, want %q", content, humanText)
	}
}

func TestTryJevSquadRouting_StateBuilderPreservesLatestHumanCommentUnderBudgetPressure(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}

	ctx := context.Background()
	wsUUID, _ := util.ParseUUID(testWorkspaceID)
	rtUUID, _ := util.ParseUUID(testRuntimeID)
	userUUID, _ := util.ParseUUID(testUserID)

	leaderAgentID := dbid.NewV7()
	memberAgentID := dbid.NewV7()
	_, err := testPool.Exec(ctx, `
		INSERT INTO agent (id, workspace_id, runtime_id, owner_id, name, status, runtime_mode)
		VALUES ($1, $2, $3, $4, 'pressure-leader', 'idle', 'cloud'),
		       ($5, $2, $3, $4, 'pressure-worker', 'idle', 'cloud')
	`, leaderAgentID, wsUUID, rtUUID, userUUID, memberAgentID)
	if err != nil {
		t.Fatalf("insert test agents: %v", err)
	}
	defer testPool.Exec(ctx, `DELETE FROM agent WHERE id IN ($1, $2)`, leaderAgentID, memberAgentID)

	squadID := dbid.NewV7()
	// Grandfathered large squad instructions (~20KB)
	largeInstructions := "Tier S: pressure-worker\n" + strings.Repeat("detailed squad guideline paragraph here.\n", 500)
	_, err = testPool.Exec(ctx, `
		INSERT INTO squad (id, workspace_id, name, description, instructions, leader_id, creator_id)
		VALUES ($1, $2, 'Pressure Squad', '', $3, $4, $5)
	`, squadID, wsUUID, largeInstructions, leaderAgentID, userUUID)
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

	var capturedState map[string]any
	mockJevServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jev.Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		capturedState, _ = req.State.(map[string]any)
		resp := jev.Response{
			Model: "jev-1.13.0",
			Answers: map[string]jev.Answer{
				"route_squad_member": {
					Type:       jev.TypeChoice,
					Choice:     util.UUIDToString(memberAgentID),
					Confidence: 0.95,
					Probabilities: map[string]float64{
						util.UUIDToString(memberAgentID): 0.95,
						"escalate_to_leader":             0.05,
					},
				},
			},
			Usage: jev.Usage{InputTokens: 500, OutputTokens: 10},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockJevServer.Close()

	var minConf = 0.90
	settings := governance.WorkspaceSettings{
		JevSquadRoutingEnabled: true,
		JevAPIURL:              mockJevServer.URL,
		JevMinConfidence:       &minConf,
	}
	settingsJSON, _ := json.Marshal(settings)
	_, _ = testPool.Exec(ctx, `
		INSERT INTO governance_workspace_config (workspace_id, config_version, control_epoch, settings)
		VALUES ($1, 1, 1, $2)
		ON CONFLICT (workspace_id) DO UPDATE SET settings = EXCLUDED.settings
	`, wsUUID, settingsJSON)

	jevClient, _ := jev.NewClient(jev.Options{
		APIKey:  "test-jev-key",
		BaseURL: mockJevServer.URL,
	})
	origJev := testHandler.Jev
	testHandler.Jev = jevClient
	defer func() { testHandler.Jev = origJev }()

	squad, _ := testHandler.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: squadID, WorkspaceID: wsUUID})

	// 12,000-character description (~4000 tokens)
	cjkDesc := strings.Repeat("功能需求描述测试详细信息", 1000)
	issueID := dbid.NewV7()
	_, _ = testPool.Exec(ctx, `
		INSERT INTO issue (id, workspace_id, title, description, status, priority, assignee_type, assignee_id, creator_type, creator_id, revision, number)
		VALUES ($1, $2, 'Pressure Test Issue', $3, 'backlog', 'high', 'squad', $4, 'member', $5, 1, 4006)
	`, issueID, wsUUID, cjkDesc, squadID, testUserID)
	defer testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, issueID)

	// 20 comments total, newest comment is human
	for i := 1; i <= 19; i++ {
		_, _ = testPool.Exec(ctx, `
			INSERT INTO comment (id, workspace_id, issue_id, author_type, author_id, content, created_at, updated_at)
			VALUES ($1, $2, $3, 'agent', $4, $5, NOW() + ($6 * interval '1 second'), NOW() + ($6 * interval '1 second'))
		`, dbid.NewV7(), wsUUID, issueID, leaderAgentID, fmt.Sprintf("Bot message %d %s", i, strings.Repeat("chatter ", 300)), i)
	}
	humanText := "Route to pressure-worker immediately"
	_, _ = testPool.Exec(ctx, `
		INSERT INTO comment (id, workspace_id, issue_id, author_type, author_id, content, created_at, updated_at)
		VALUES ($1, $2, $3, 'member', $4, $5, NOW() + (20 * interval '1 second'), NOW() + (20 * interval '1 second'))
	`, dbid.NewV7(), wsUUID, issueID, testUserID, humanText)

	issue, _ := testHandler.Queries.GetIssue(ctx, issueID)
	routed, adv := testHandler.tryJevSquadRouting(ctx, issue, squad, "member", testUserID, testUserID)
	if !routed {
		t.Fatalf("expected routing = true under budget pressure, got false: %s", adv)
	}

	if capturedState == nil {
		t.Fatalf("expected capturedState not to be nil")
	}
	lhc, ok := capturedState["latest_human_comment"].(map[string]any)
	if !ok || lhc == nil {
		t.Fatalf("expected latest_human_comment to survive budget pressure as PriorityRequired")
	}
	if content, _ := lhc["content"].(string); content != humanText {
		t.Errorf("latest_human_comment content = %q, want %q", content, humanText)
	}
}

func TestTryJevSquadRouting_MandatorySectionCannotFitFailsClosed(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}

	ctx := context.Background()
	wsUUID, _ := util.ParseUUID(testWorkspaceID)
	rtUUID, _ := util.ParseUUID(testRuntimeID)
	userUUID, _ := util.ParseUUID(testUserID)

	leaderAgentID := dbid.NewV7()
	memberAgentID := dbid.NewV7()
	_, err := testPool.Exec(ctx, `
		INSERT INTO agent (id, workspace_id, runtime_id, owner_id, name, status, runtime_mode)
		VALUES ($1, $2, $3, $4, 'overflow-leader', 'idle', 'cloud'),
		       ($5, $2, $3, $4, 'overflow-worker', 'idle', 'cloud')
	`, leaderAgentID, wsUUID, rtUUID, userUUID, memberAgentID)
	if err != nil {
		t.Fatalf("insert test agents: %v", err)
	}
	defer testPool.Exec(ctx, `DELETE FROM agent WHERE id IN ($1, $2)`, leaderAgentID, memberAgentID)

	squadID := dbid.NewV7()
	_, err = testPool.Exec(ctx, `
		INSERT INTO squad (id, workspace_id, name, description, instructions, leader_id, creator_id)
		VALUES ($1, $2, 'Overflow Squad', '', 'Tier S: overflow-worker', $3, $4)
	`, squadID, wsUUID, leaderAgentID, userUUID)
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

	var minConf = 0.90
	settings := governance.WorkspaceSettings{
		JevSquadRoutingEnabled: true,
		JevAPIURL:              "http://127.0.0.1:9",
		JevMinConfidence:       &minConf,
	}
	settingsJSON, _ := json.Marshal(settings)
	_, _ = testPool.Exec(ctx, `
		INSERT INTO governance_workspace_config (workspace_id, config_version, control_epoch, settings)
		VALUES ($1, 1, 1, $2)
		ON CONFLICT (workspace_id) DO UPDATE SET settings = EXCLUDED.settings
	`, wsUUID, settingsJSON)

	squad, _ := testHandler.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: squadID, WorkspaceID: wsUUID})

	jevClient, _ := jev.NewClient(jev.Options{
		APIKey:  "test-jev-key",
		BaseURL: "http://127.0.0.1:9",
	})
	origJev := testHandler.Jev
	testHandler.Jev = jevClient
	defer func() { testHandler.Jev = origJev }()

	issueID := dbid.NewV7()
	_, _ = testPool.Exec(ctx, `
		INSERT INTO issue (id, workspace_id, title, description, status, priority, assignee_type, assignee_id, creator_type, creator_id, revision, number)
		VALUES ($1, $2, 'Overflow Test Issue', 'Issue testing budget overflow', 'backlog', 'high', 'squad', $3, 'member', $4, 1, 4007)
	`, issueID, wsUUID, squadID, testUserID)
	defer testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, issueID)

	// Insert human comment exceeding 14,000 tokens (>42,000 characters)
	hugeContent := strings.Repeat("over budget comment text ", 2500) // ~60,000 chars, ~20,000 tokens
	_, err = testPool.Exec(ctx, `
		INSERT INTO comment (id, workspace_id, issue_id, author_type, author_id, content, created_at, updated_at)
		VALUES ($1, $2, $3, 'member', $4, $5, NOW(), NOW())
	`, dbid.NewV7(), wsUUID, issueID, testUserID, hugeContent)
	if err != nil {
		t.Fatalf("insert huge comment: %v", err)
	}

	issue, _ := testHandler.Queries.GetIssue(ctx, issueID)
	routed, adv := testHandler.tryJevSquadRouting(ctx, issue, squad, "member", testUserID, testUserID)
	if routed {
		t.Fatalf("expected routed = false when human comment exceeds budget")
	}
	if !strings.Contains(adv, "exceeds comment budget") {
		t.Errorf("advisory missing budget error: %s", adv)
	}
}

func TestCreateIssue_SquadAssigneeTriggersJevRouting(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}

	ctx := context.Background()
	wsUUID, _ := util.ParseUUID(testWorkspaceID)
	rtUUID, _ := util.ParseUUID(testRuntimeID)
	userUUID, _ := util.ParseUUID(testUserID)

	leaderAgentID := dbid.NewV7()
	memberAgentID := dbid.NewV7()
	_, err := testPool.Exec(ctx, `
		INSERT INTO agent (id, workspace_id, runtime_id, owner_id, name, status, runtime_mode)
		VALUES ($1, $2, $3, $4, 'create-leader', 'idle', 'cloud'),
		       ($5, $2, $3, $4, 'create-worker', 'idle', 'cloud')
	`, leaderAgentID, wsUUID, rtUUID, userUUID, memberAgentID)
	if err != nil {
		t.Fatalf("insert test agents: %v", err)
	}
	defer testPool.Exec(ctx, `DELETE FROM agent WHERE id IN ($1, $2)`, leaderAgentID, memberAgentID)

	squadID := dbid.NewV7()
	_, err = testPool.Exec(ctx, `
		INSERT INTO squad (id, workspace_id, name, description, instructions, leader_id, creator_id)
		VALUES ($1, $2, 'Create Test Squad', '', 'Tier S: create-worker', $3, $4)
	`, squadID, wsUUID, leaderAgentID, userUUID)
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

	mockJevServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := jev.Response{
			Model: "jev-1.13.0",
			Answers: map[string]jev.Answer{
				"route_squad_member": {
					Type:       jev.TypeChoice,
					Choice:     util.UUIDToString(memberAgentID),
					Confidence: 0.95,
					Probabilities: map[string]float64{
						util.UUIDToString(memberAgentID): 0.95,
						"escalate_to_leader":             0.05,
					},
				},
			},
			Usage: jev.Usage{InputTokens: 500, OutputTokens: 10},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockJevServer.Close()

	var minConf = 0.90
	settings := governance.WorkspaceSettings{
		JevSquadRoutingEnabled: true,
		JevAPIURL:              mockJevServer.URL,
		JevMinConfidence:       &minConf,
	}
	settingsJSON, _ := json.Marshal(settings)
	_, _ = testPool.Exec(ctx, `
		INSERT INTO governance_workspace_config (workspace_id, config_version, control_epoch, settings)
		VALUES ($1, 1, 1, $2)
		ON CONFLICT (workspace_id) DO UPDATE SET settings = EXCLUDED.settings
	`, wsUUID, settingsJSON)

	jevClient, _ := jev.NewClient(jev.Options{
		APIKey:  "test-jev-key",
		BaseURL: mockJevServer.URL,
	})
	origJev := testHandler.Jev
	testHandler.Jev = jevClient
	defer func() { testHandler.Jev = origJev }()

	// Create issue with squad assignee via IssueService
	createResult, err := testHandler.IssueService.Create(ctx, service.IssueCreateParams{
		WorkspaceID:  wsUUID,
		Title:        "Issue with Squad Assignee",
		Description:  pgtype.Text{String: "Needs specialist execution", Valid: true},
		Status:       "todo",
		Priority:     "high",
		AssigneeType: pgtype.Text{String: "squad", Valid: true},
		AssigneeID:   squadID,
		CreatorType:  "member",
		CreatorID:    userUUID,
	}, service.IssueCreateOpts{})
	if err != nil {
		t.Fatalf("CreateIssue failed: %v", err)
	}
	defer testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, createResult.Issue.ID)

	// Verify issue was updated to in_progress
	freshIssue, err := testHandler.Queries.GetIssue(ctx, createResult.Issue.ID)
	if err != nil {
		t.Fatalf("GetIssue failed: %v", err)
	}
	if freshIssue.Status != "in_progress" {
		t.Errorf("issue status = %q, want in_progress", freshIssue.Status)
	}

	// Verify task exists for create-worker (memberAgentID)
	var enqueuedAgentID string
	err = testPool.QueryRow(ctx, `
		SELECT agent_id FROM agent_task_queue WHERE issue_id = $1 ORDER BY created_at DESC LIMIT 1
	`, createResult.Issue.ID).Scan(&enqueuedAgentID)
	if err != nil {
		t.Fatalf("task not found in queue: %v", err)
	}
	if enqueuedAgentID != util.UUIDToString(memberAgentID) {
		t.Errorf("enqueued task agent_id = %s, want worker %s", enqueuedAgentID, util.UUIDToString(memberAgentID))
	}

	// Verify jev_route announcement comment exists
	var commentType string
	err = testPool.QueryRow(ctx, `
		SELECT type FROM comment WHERE issue_id = $1 AND type = 'jev_route' LIMIT 1
	`, createResult.Issue.ID).Scan(&commentType)
	if err != nil || commentType != "jev_route" {
		t.Errorf("expected jev_route comment on issue, got err=%v, type=%s", err, commentType)
	}
}

func TestComment_SquadMentionTriggersJevRouting(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}

	ctx := context.Background()
	wsUUID, _ := util.ParseUUID(testWorkspaceID)
	rtUUID, _ := util.ParseUUID(testRuntimeID)
	userUUID, _ := util.ParseUUID(testUserID)

	leaderAgentID := dbid.NewV7()
	memberAgentID := dbid.NewV7()
	_, err := testPool.Exec(ctx, `
		INSERT INTO agent (id, workspace_id, runtime_id, owner_id, name, status, runtime_mode)
		VALUES ($1, $2, $3, $4, 'mention-leader', 'idle', 'cloud'),
		       ($5, $2, $3, $4, 'mention-worker', 'idle', 'cloud')
	`, leaderAgentID, wsUUID, rtUUID, userUUID, memberAgentID)
	if err != nil {
		t.Fatalf("insert test agents: %v", err)
	}
	defer testPool.Exec(ctx, `DELETE FROM agent WHERE id IN ($1, $2)`, leaderAgentID, memberAgentID)

	squadID := dbid.NewV7()
	_, err = testPool.Exec(ctx, `
		INSERT INTO squad (id, workspace_id, name, description, instructions, leader_id, creator_id)
		VALUES ($1, $2, 'Mention Squad', '', 'Tier S: mention-worker', $3, $4)
	`, squadID, wsUUID, leaderAgentID, userUUID)
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

	mockJevServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := jev.Response{
			Model: "jev-1.13.0",
			Answers: map[string]jev.Answer{
				"route_squad_member": {
					Type:       jev.TypeChoice,
					Choice:     util.UUIDToString(memberAgentID),
					Confidence: 0.95,
					Probabilities: map[string]float64{
						util.UUIDToString(memberAgentID): 0.95,
						"escalate_to_leader":             0.05,
					},
				},
			},
			Usage: jev.Usage{InputTokens: 500, OutputTokens: 10},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockJevServer.Close()

	var minConf = 0.90
	settings := governance.WorkspaceSettings{
		JevSquadRoutingEnabled: true,
		JevAPIURL:              mockJevServer.URL,
		JevMinConfidence:       &minConf,
	}
	settingsJSON, _ := json.Marshal(settings)
	_, _ = testPool.Exec(ctx, `
		INSERT INTO governance_workspace_config (workspace_id, config_version, control_epoch, settings)
		VALUES ($1, 1, 1, $2)
		ON CONFLICT (workspace_id) DO UPDATE SET settings = EXCLUDED.settings
	`, wsUUID, settingsJSON)

	jevClient, _ := jev.NewClient(jev.Options{
		APIKey:  "test-jev-key",
		BaseURL: mockJevServer.URL,
	})
	origJev := testHandler.Jev
	testHandler.Jev = jevClient
	defer func() { testHandler.Jev = origJev }()

	squad, _ := testHandler.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: squadID, WorkspaceID: wsUUID})
	leaderAgent, _ := testHandler.Queries.GetAgent(ctx, leaderAgentID)

	issueID := dbid.NewV7()
	_, _ = testPool.Exec(ctx, `
		INSERT INTO issue (id, workspace_id, title, description, status, priority, assignee_type, assignee_id, creator_type, creator_id, revision, number)
		VALUES ($1, $2, 'Comment Trigger Issue', 'Issue testing comment squad mention trigger', 'backlog', 'high', 'squad', $3, 'member', $4, 1, 4008)
	`, issueID, wsUUID, squadID, testUserID)
	defer testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, issueID)

	commentID := dbid.NewV7()
	_, _ = testPool.Exec(ctx, `
		INSERT INTO comment (id, workspace_id, issue_id, author_type, author_id, content, created_at, updated_at)
		VALUES ($1, $2, $3, 'member', $4, 'Hey @Mention Squad please handle this task', NOW(), NOW())
	`, commentID, wsUUID, issueID, testUserID)
	defer testPool.Exec(ctx, `DELETE FROM comment WHERE id = $1`, commentID)

	issue, _ := testHandler.Queries.GetIssue(ctx, issueID)
	trigger := commentAgentTrigger{
		Agent:  leaderAgent,
		Squad:  &squad,
		Source: commentTriggerSourceMentionSquadLeader,
	}

	_, err = testHandler.enqueueSingleCommentTrigger(ctx, issue, commentID, trigger)
	if err != nil {
		t.Fatalf("enqueueSingleCommentTrigger failed: %v", err)
	}

	// Verify issue was promoted to in_progress
	freshIssue, _ := testHandler.Queries.GetIssue(ctx, issueID)
	if freshIssue.Status != "in_progress" {
		t.Errorf("issue status = %q, want in_progress", freshIssue.Status)
	}

	// Verify task exists for worker agent
	var enqueuedAgentID string
	err = testPool.QueryRow(ctx, `
		SELECT agent_id FROM agent_task_queue WHERE issue_id = $1 ORDER BY created_at DESC LIMIT 1
	`, issueID).Scan(&enqueuedAgentID)
	if err != nil {
		t.Fatalf("task not found in queue: %v", err)
	}
	if enqueuedAgentID != util.UUIDToString(memberAgentID) {
		t.Errorf("enqueued task agent_id = %s, want worker %s", enqueuedAgentID, util.UUIDToString(memberAgentID))
	}

	// Verify jev_route comment created
	var commentType string
	err = testPool.QueryRow(ctx, `
		SELECT type FROM comment WHERE issue_id = $1 AND type = 'jev_route' LIMIT 1
	`, issueID).Scan(&commentType)
	if err != nil || commentType != "jev_route" {
		t.Errorf("expected jev_route comment on issue, got err=%v, type=%s", err, commentType)
	}
}
