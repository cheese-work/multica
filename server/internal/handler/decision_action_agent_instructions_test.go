package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Only the governed:agent region may change while a governed:human block exists.
const governedAgentText = "<!-- governed:agent -->role: author<!-- /governed:agent -->\n<!-- governed:human -->never merge<!-- /governed:human -->\ntail"

// agentInstructionRequest is one pending agent.instructions.set action on the
// fixture agent, whose live instructions are governedAgentText.
type agentInstructionRequest struct {
	action pendingAction
	// target is the agent whose instructions the action changes; it is not the
	// asking agent when a second request is filed on the same target.
	target string
	before string
	after  string
}

// requestAgentInstructions files the request; edit may break the params first.
func requestAgentInstructions(test *testing.T, status int, edit func(params map[string]any)) (agentInstructionRequest, string) {
	test.Helper()
	fixture := newActionFixture(test)
	dbfx.Exec(test, `UPDATE agent SET instructions = $1 WHERE id = $2`, governedAgentText, fixture.agentID)
	after := strings.Replace(governedAgentText, "role: author", "role: reviewer", 1)
	params := map[string]any{
		"agent_id": fixture.agentID, "instructions": after,
		"base_sha256": decisionaction.TextDigest(governedAgentText), "after_sha256": decisionaction.TextDigest(after),
	}
	if edit != nil {
		edit(params)
	}
	var created actionResponse
	body := fixture.respond(test, fixture.withAction(map[string]any{"type": "agent.instructions.set", "version": 1, "params": params}), status, &created)
	if status != http.StatusCreated {
		if requests, actions := fixture.requestCounts(test); requests != 0 || actions != 0 {
			test.Fatalf("refused request stored %d requests and %d actions; want none", requests, actions)
		}
		return agentInstructionRequest{before: governedAgentText, after: after}, body
	}
	dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
	return agentInstructionRequest{pendingAction{fixture, created.ID, created.Action.ID}, fixture.agentID, governedAgentText, after}, body
}

// requestAgentInstructionsOn files a second request against target, built on
// the same base digest as the first.
func requestAgentInstructionsOn(test *testing.T, target, role string) agentInstructionRequest {
	test.Helper()
	fixture := newActionFixture(test)
	after := strings.Replace(governedAgentText, "role: author", "role: "+role, 1)
	params := map[string]any{
		"agent_id": target, "instructions": after,
		"base_sha256": decisionaction.TextDigest(governedAgentText), "after_sha256": decisionaction.TextDigest(after),
	}
	var created actionResponse
	fixture.respond(test, fixture.withAction(map[string]any{"type": "agent.instructions.set", "version": 1, "params": params}), http.StatusCreated, &created)
	dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
	return agentInstructionRequest{pendingAction{fixture, created.ID, created.Action.ID}, target, governedAgentText, after}
}

func (request agentInstructionRequest) live(test *testing.T) string {
	test.Helper()
	var text string
	dbfx.QueryRow(test, `SELECT instructions FROM agent WHERE id = $1`, request.target).Scan(&text)
	return text
}

func (request agentInstructionRequest) claim(test *testing.T) map[string]any {
	test.Helper()
	var preview string
	dbfx.QueryRow(test, `SELECT preconditions->>'preview_sha256' FROM decision_action WHERE id = $1`, request.action.actionID).Scan(&preview)
	claim := request.action.claim(test, map[string]string{})
	claim["preview_sha256"] = preview
	return claim
}

// approveOwner approves as the owner with the preview digest and expects status.
func (request agentInstructionRequest) approveOwner(test *testing.T, handler *Handler, status int) string {
	test.Helper()
	makeApproverOwner(test, request.action, "owner")
	return request.action.approve(test, handler, request.claim(test), status)
}

// assertNotApplied proves a refused approval changed nothing.
func (request agentInstructionRequest) assertNotApplied(test *testing.T, want string) {
	test.Helper()
	request.action.assertUntouched(test)
	if got := request.live(test); got != want {
		test.Fatalf("instructions %q, want unchanged %q", got, want)
	}
	if n := request.action.activityCount(test, "config_change_applied"); n != 0 {
		test.Fatalf("config_change_applied rows = %d, want 0", n)
	}
}

func TestAgentInstructionsSetIsRegisteredAsAnOwnerOnlyPreviewedAgentAction(test *testing.T) {
	definition, found := decisionaction.Lookup("agent.instructions.set", 1)
	if !found {
		test.Fatal("agent.instructions.set@1 is not registered")
	}
	if definition.Target != decisionaction.TargetAgent || !definition.OwnerOnly || !definition.RequiresPreview {
		test.Fatalf("definition %+v; want agent target, owner only, requires preview", definition)
	}
	if _, found := configChanges["agent.instructions.set"]; !found {
		test.Fatal("agent.instructions.set has no config-change core")
	}
}

func TestAgentInstructionsSetRequestStoresTheServerPreview(test *testing.T) {
	request, _ := requestAgentInstructions(test, http.StatusCreated, nil)
	var raw string
	dbfx.QueryRow(test, `SELECT preconditions::text FROM decision_action WHERE id = $1`, request.action.actionID).Scan(&raw)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		test.Fatal(err)
	}
	var field, target, before, after string
	_ = json.Unmarshal(fields["field"], &field)
	_ = json.Unmarshal(fields["target_id"], &target)
	_ = json.Unmarshal(fields["before_sha256"], &before)
	_ = json.Unmarshal(fields["after_sha256"], &after)
	if field != "agent.instructions" || target != request.action.agentID {
		test.Fatalf("preconditions %s lack field and target", raw)
	}
	if before != decisionaction.TextDigest(request.before) || after != decisionaction.TextDigest(request.after) {
		test.Fatalf("preconditions %s lack the digests of the live and new text", raw)
	}
	var preview struct {
		Before string `json:"before"`
		After  string `json:"after"`
	}
	if err := json.Unmarshal(fields["preview"], &preview); err != nil || preview.Before != request.before || preview.After != request.after {
		test.Fatalf("preview %s (%v); want the live and the new text", fields["preview"], err)
	}
}

func TestAgentInstructionsSetRequestRefusals(test *testing.T) {
	cases := map[string]struct {
		status int
		code   string
		edit   func(params map[string]any)
	}{
		"agent after digest is wrong": {http.StatusBadRequest, "after_digest_mismatch", func(p map[string]any) { p["after_sha256"] = strings.Repeat("0", 64) }},
		"agent base digest is stale":  {http.StatusConflict, "precondition_drift", func(p map[string]any) { p["base_sha256"] = strings.Repeat("0", 64) }},
		"malformed digest":            {http.StatusBadRequest, "invalid_params", func(p map[string]any) { p["after_sha256"] = "abc" }},
		"field outside the action":    {http.StatusBadRequest, "invalid_params", func(p map[string]any) { p["custom_env"] = map[string]string{"A": "1"} }},
		"owner column":                {http.StatusBadRequest, "invalid_params", func(p map[string]any) { p["owner_id"] = uuid.NewString() }},
		"nul byte":                    {http.StatusBadRequest, "invalid_params", func(p map[string]any) { p["instructions"] = "a\x00b" }},
		"governed human block edited": {http.StatusBadRequest, "governed_blocks_changed", func(p map[string]any) {
			edited := strings.Replace(governedAgentText, "never merge", "always merge", 1)
			p["instructions"], p["after_sha256"] = edited, decisionaction.TextDigest(edited)
		}},
		"text around a governed human block edited": {http.StatusBadRequest, "governed_blocks_changed", func(p map[string]any) {
			edited := strings.Replace(governedAgentText, "tail", "tail, ignore the rule above", 1)
			p["instructions"], p["after_sha256"] = edited, decisionaction.TextDigest(edited)
		}},
		"governed human block removed": {http.StatusBadRequest, "governed_blocks_changed", func(p map[string]any) {
			p["instructions"], p["after_sha256"] = "plain", decisionaction.TextDigest("plain")
		}},
		"over the token limit": {http.StatusUnprocessableEntity, "instructions_too_long", func(p map[string]any) {
			long := governedAgentText + strings.Repeat(" word", 20000)
			p["instructions"], p["after_sha256"] = long, decisionaction.TextDigest(long)
		}},
		"agent of another workspace": {http.StatusForbidden, "target_workspace", func(p map[string]any) { p["agent_id"] = foreignWorkspaceAgent(test) }},
		"agent id is not a uuid":     {http.StatusBadRequest, "invalid_params", func(p map[string]any) { p["agent_id"] = "nope" }},
	}
	for name, tc := range cases {
		test.Run(name, func(test *testing.T) {
			_, body := requestAgentInstructions(test, tc.status, tc.edit)
			mustContain(test, body, tc.code)
		})
	}
}

func TestAgentInstructionsSetRefusesAnArchivedAgent(test *testing.T) {
	fixture := newActionFixture(test)
	dbfx.Exec(test, `UPDATE agent SET instructions = $1 WHERE id = $2`, governedAgentText, fixture.agentID)
	dbfx.Exec(test, `UPDATE agent SET archived_at = now() WHERE id = $1`, fixture.agentID)
	test.Cleanup(func() { dbfx.Exec(test, `UPDATE agent SET archived_at = NULL WHERE id = $1`, fixture.agentID) })
	params := map[string]any{"agent_id": fixture.agentID, "instructions": "x", "base_sha256": decisionaction.TextDigest(governedAgentText), "after_sha256": decisionaction.TextDigest("x")}
	body := fixture.respond(test, fixture.withAction(map[string]any{"type": "agent.instructions.set", "version": 1, "params": params}), http.StatusConflict, nil)
	mustContain(test, body, "agent_archived")
}

func TestAgentInstructionsSetAllowsShrinkingAnOversizedValue(test *testing.T) {
	fixture := newActionFixture(test)
	wrap := func(words int) string {
		return "<!-- governed:human -->keep<!-- /governed:human -->\n<!-- governed:agent -->" + strings.Repeat("word ", words) + "<!-- /governed:agent -->"
	}
	huge, smaller := wrap(20000), wrap(10000)
	dbfx.Exec(test, `UPDATE agent SET instructions = $1 WHERE id = $2`, huge, fixture.agentID)
	params := map[string]any{"agent_id": fixture.agentID, "instructions": smaller, "base_sha256": decisionaction.TextDigest(huge), "after_sha256": decisionaction.TextDigest(smaller)}
	// Still over the limit, but smaller than the live value: allowed.
	fixture.respond(test, fixture.withAction(map[string]any{"type": "agent.instructions.set", "version": 1, "params": params}), http.StatusCreated, nil)
}

func foreignWorkspaceAgent(test *testing.T) string {
	test.Helper()
	workspace := foreignWorkspaceID(test)
	var runtimeID, agentID string
	dbfx.QueryRow(test, `SELECT id FROM agent_runtime WHERE workspace_id = $1 LIMIT 1`, testWorkspaceID).Scan(&runtimeID)
	dbfx.QueryRow(test, `INSERT INTO agent (workspace_id, name, description, runtime_mode, runtime_config, runtime_id, visibility, instructions) VALUES ($1, 'foreign agent', '', 'local', '{}', $2, 'workspace', '') RETURNING id`, workspace, runtimeID).Scan(&agentID)
	dbfx.Cleanup(test, `DELETE FROM agent WHERE id = $1`, agentID)
	return agentID
}

func TestAgentInstructionsSetApproveAppliesAuditsAndPublishes(test *testing.T) {
	request, _ := requestAgentInstructions(test, http.StatusCreated, nil)
	device, keyID := request.action.enrollPasskey(test)
	makeApproverOwner(test, request.action, "owner")
	handler := passkeyHandler(test)
	claim := request.claim(test)
	claim["assertion"] = request.action.assertion(test, device, request.action.issue(test, handler), passkeyStart+1)
	request.action.approve(test, handler, claim, http.StatusOK)
	var used string
	dbfx.QueryRow(test, `SELECT webauthn_credential_id::text FROM decision_action WHERE id = $1`, request.action.actionID).Scan(&used)
	if used != keyID {
		test.Fatalf("recorded credential %s, want %s", used, keyID)
	}

	if got := request.live(test); got != request.after {
		test.Fatalf("instructions %q, want %q", got, request.after)
	}
	var raw string
	dbfx.QueryRow(test, `SELECT result::text FROM decision_action WHERE id = $1`, request.action.actionID).Scan(&raw)
	var result map[string]any
	if err := json.Unmarshal([]byte(raw), &result); err != nil || result["readback_sha256"] != decisionaction.TextDigest(request.after) {
		test.Fatalf("result %s (%v); want readback_sha256 %s", raw, err, decisionaction.TextDigest(request.after))
	}
	dbfx.QueryRow(test, `SELECT details::text FROM activity_log WHERE issue_id = $1 AND action = 'config_change_applied'`, request.action.issueID).Scan(&raw)
	var details map[string]any
	if err := json.Unmarshal([]byte(raw), &details); err != nil {
		test.Fatal(err)
	}
	for key, want := range map[string]string{
		"field": "agent.instructions", "target_id": request.action.agentID,
		"before_sha256": decisionaction.TextDigest(request.before), "after_sha256": decisionaction.TextDigest(request.after),
		"decision_request_id": request.action.requestID, "decision_action_id": request.action.actionID, "credential_id": keyID,
	} {
		if details[key] != want {
			test.Fatalf("audit %s = %v, want %s (details %s)", key, details[key], want, raw)
		}
	}
	if strings.Contains(raw, "never merge") {
		test.Fatalf("audit row carries instruction text: %s", raw)
	}
}

func TestAgentInstructionsSetApproveRefusals(test *testing.T) {
	test.Run("live value changed after the request", func(test *testing.T) {
		request, _ := requestAgentInstructions(test, http.StatusCreated, nil)
		drifted := strings.Replace(governedAgentText, "role: author", "role: someone else", 1)
		dbfx.Exec(test, `UPDATE agent SET instructions = $1 WHERE id = $2`, drifted, request.action.agentID)
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "precondition_drift")
		request.assertNotApplied(test, drifted)
	})
	test.Run("governed block changed after the request", func(test *testing.T) {
		request, _ := requestAgentInstructions(test, http.StatusCreated, nil)
		changed := strings.Replace(governedAgentText, "never merge", "may merge", 1)
		dbfx.Exec(test, `UPDATE agent SET instructions = $1 WHERE id = $2`, changed, request.action.agentID)
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "governed_blocks_changed")
		request.assertNotApplied(test, changed)
	})
	test.Run("approver is an admin", func(test *testing.T) {
		request, _ := requestAgentInstructions(test, http.StatusCreated, nil)
		makeApproverOwner(test, request.action, "admin")
		mustContain(test, request.action.approve(test, withVerifier(acceptingVerifier), request.claim(test), http.StatusForbidden), "class_needs_owner")
		request.assertNotApplied(test, request.before)
	})
	test.Run("owner demoted during verification", func(test *testing.T) {
		request, _ := requestAgentInstructions(test, http.StatusCreated, nil)
		makeApproverOwner(test, request.action, "owner")
		demote := func(context.Context, *db.Queries, ApprovalClaim) (pgtype.UUID, error) {
			makeApproverOwner(test, request.action, "admin")
			return pgtype.UUID{}, nil
		}
		mustContain(test, request.action.approve(test, withVerifier(demote), request.claim(test), http.StatusForbidden), "class_needs_owner")
		request.assertNotApplied(test, request.before)
	})
	test.Run("payload digest altered", func(test *testing.T) {
		request, _ := requestAgentInstructions(test, http.StatusCreated, nil)
		makeApproverOwner(test, request.action, "owner")
		claim := request.claim(test)
		claim["payload_sha256"] = strings.Repeat("0", 64)
		mustContain(test, request.action.approve(test, withVerifier(acceptingVerifier), claim, http.StatusConflict), "payload_mismatch")
		request.assertNotApplied(test, request.before)
	})
	test.Run("preview digest altered", func(test *testing.T) {
		request, _ := requestAgentInstructions(test, http.StatusCreated, nil)
		makeApproverOwner(test, request.action, "owner")
		claim := request.claim(test)
		claim["preview_sha256"] = strings.Repeat("0", 64)
		mustContain(test, request.action.approve(test, withVerifier(acceptingVerifier), claim, http.StatusConflict), "preview_required")
		request.assertNotApplied(test, request.before)
	})
	test.Run("expired", func(test *testing.T) {
		request, _ := requestAgentInstructions(test, http.StatusCreated, nil)
		dbfx.Exec(test, `UPDATE decision_action SET expires_at = now() - interval '1 minute' WHERE id = $1`, request.action.actionID)
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "expired")
		request.assertNotApplied(test, request.before)
	})
	test.Run("no assertion", func(test *testing.T) {
		request, _ := requestAgentInstructions(test, http.StatusCreated, nil)
		request.action.enrollPasskey(test)
		makeApproverOwner(test, request.action, "owner")
		claim := request.claim(test)
		claim["assertion"] = nil
		request.action.approve(test, passkeyHandler(test), claim, http.StatusForbidden)
		request.assertNotApplied(test, request.before)
	})
	test.Run("assertion signed over another challenge", func(test *testing.T) {
		request, _ := requestAgentInstructions(test, http.StatusCreated, nil)
		device, _ := request.action.enrollPasskey(test)
		makeApproverOwner(test, request.action, "owner")
		handler := passkeyHandler(test)
		issued := request.action.issue(test, handler)
		issued.Options.PublicKey.Challenge = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		claim := request.claim(test)
		claim["assertion"] = request.action.assertion(test, device, issued, passkeyStart+1)
		request.action.approve(test, handler, claim, http.StatusForbidden)
		request.assertNotApplied(test, request.before)
	})
	test.Run("target moved to another workspace", func(test *testing.T) {
		request, _ := requestAgentInstructions(test, http.StatusCreated, nil)
		dbfx.Exec(test, `UPDATE decision_action SET params = jsonb_set(params, '{agent_id}', to_jsonb($1::text)) WHERE id = $2`, foreignWorkspaceAgent(test), request.action.actionID)
		body := request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict)
		if !strings.Contains(body, "payload_mismatch") && !strings.Contains(body, "target_") {
			test.Fatalf("body %q; want a refusal of the foreign target", body)
		}
		request.assertNotApplied(test, request.before)
	})
}

func TestAgentInstructionsSetRunsOnce(test *testing.T) {
	request, _ := requestAgentInstructions(test, http.StatusCreated, nil)
	handler := withVerifier(acceptingVerifier)
	request.approveOwner(test, handler, http.StatusOK)
	request.action.approve(test, handler, request.claim(test), http.StatusConflict)
	if n := request.action.activityCount(test, "config_change_applied"); n != 1 {
		test.Fatalf("config_change_applied rows = %d after a replay, want 1", n)
	}
}

func TestAgentInstructionsSetBusyTargetAnswersBusyRetry(test *testing.T) {
	test.Run("another approval holds the field lock", func(test *testing.T) {
		request, _ := requestAgentInstructions(test, http.StatusCreated, nil)
		holdAdvisoryLock(test, configChangeLockKey(testWorkspaceID, agentInstructionsField, request.action.agentID))
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "busy_retry")
		request.assertNotApplied(test, request.before)
	})
	test.Run("an ordinary edit holds the agent row", func(test *testing.T) {
		request, _ := requestAgentInstructions(test, http.StatusCreated, nil)
		holdRow(test, `UPDATE agent SET description = 'ordinary edit' WHERE id = $1`, request.action.agentID)
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "busy_retry")
		request.assertNotApplied(test, request.before)
	})
}

// A second request on the same target and base digest, approved after the
// first one applied, is refused as drift and changes nothing.
func TestAgentInstructionsSetSecondApprovalOnTheSameBaseIsDrift(test *testing.T) {
	first, _ := requestAgentInstructions(test, http.StatusCreated, nil)
	second := requestAgentInstructionsOn(test, first.target, "planner")
	handler := withVerifier(acceptingVerifier)
	first.approveOwner(test, handler, http.StatusOK)
	mustContain(test, second.approveOwner(test, handler, http.StatusConflict), "precondition_drift")
	second.action.assertUntouched(test)
	if got := first.live(test); got != first.after {
		test.Fatalf("instructions %q after the losing approval, want the winner's %q", got, first.after)
	}
	if n := second.action.activityCount(test, "config_change_applied"); n != 0 {
		test.Fatalf("losing action wrote %d config_change_applied rows, want 0", n)
	}
}

// Two approvals racing on one target apply exactly once. The approvals queue
// on the field lock, so the loser reads the winner's committed value and
// answers precondition_drift, never busy_retry. The loser never mutates.
func TestAgentInstructionsSetConcurrentApprovalsApplyOnce(test *testing.T) {
	first, _ := requestAgentInstructions(test, http.StatusCreated, nil)
	second := requestAgentInstructionsOn(test, first.target, "planner")
	makeApproverOwner(test, first.action, "owner")
	makeApproverOwner(test, second.action, "owner")
	handler := withVerifier(acceptingVerifier)
	requests := []agentInstructionRequest{first, second}
	claims := []map[string]any{first.claim(test), second.claim(test)}
	bodies, statuses := make([]string, 2), make([]int, 2)
	var group sync.WaitGroup
	for i, request := range requests {
		group.Add(1)
		go func() {
			defer group.Done()
			statuses[i], bodies[i] = approveClaim(test, handler, request, claims[i])
		}()
	}
	group.Wait()
	winner, loser := 0, 1
	if statuses[1] == http.StatusOK {
		winner, loser = 1, 0
	}
	if statuses[winner] != http.StatusOK || statuses[loser] != http.StatusConflict {
		test.Fatalf("statuses %v (%q, %q); want one 200 and one 409", statuses, bodies[0], bodies[1])
	}
	if !strings.Contains(bodies[loser], "precondition_drift") {
		test.Fatalf("loser body %q; want precondition_drift", bodies[loser])
	}
	applied := dbfx.Count(test, `SELECT count(*) FROM activity_log WHERE issue_id IN ($1, $2) AND action = 'config_change_applied'`, first.action.issueID, second.action.issueID)
	if applied != 1 {
		test.Fatalf("applied %d times (statuses %v), want exactly 1", applied, statuses)
	}
	if got := first.live(test); got != requests[winner].after {
		test.Fatalf("instructions %q, want the winner's %q", got, requests[winner].after)
	}
	requests[loser].action.assertUntouched(test)
	mustContain(test, requests[loser].action.approve(test, handler, requests[loser].claim(test), http.StatusConflict), "precondition_drift")
	if got := first.live(test); got != requests[winner].after {
		test.Fatalf("the loser's retry changed the instructions to %q", got)
	}
}

// approveClaim posts an approval and returns its status and body; it is safe to
// call from a goroutine.
func approveClaim(test *testing.T, handler *Handler, request agentInstructionRequest, claim map[string]any) (int, string) {
	test.Helper()
	raw, err := json.Marshal(claim)
	if err != nil {
		test.Error(err)
		return 0, ""
	}
	call := request.action.decide("approve", request.action.humanID, "interactive_jwt")
	call.Body = readerCloser{bytes.NewReader(raw)}
	recorder := httptest.NewRecorder()
	handler.ApproveDecisionAction(recorder, call)
	return recorder.Code, recorder.Body.String()
}
