package handler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-764: a single, human-approved exception to CHE-455 letting exactly
// agent hermesExceptionAgentID write workspace.context on
// hermesExceptionWorkspaceID and squad.instructions on hermesExceptionSquadID,
// via an exact-bytes compare-and-swap keyed on a sha256 expected-before
// digest. See governed_instruction_hermes_exception.go for full rationale
// and provenance. These tests exercise the real HTTP handlers end-to-end
// using fixture rows seeded with the exact production literal IDs the
// exception hard-codes, rather than unit-testing the predicate in isolation,
// so a regression in the wiring (not just the predicate) would be caught.

func testDigest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum)
}

// hermesExceptionFixture seeds an isolated workspace/owner/member/runtime,
// then an agent row whose id is EXACTLY hermesExceptionAgentID and a
// workspace row whose id is EXACTLY hermesExceptionWorkspaceID, so requests
// exercise the real hardcoded-constant comparison instead of a look-alike.
// Returns the owner user id, the agent id (== hermesExceptionAgentID), and a
// task id bound to that agent so tests can authenticate as it via
// asAgentActor.
func hermesExceptionFixture(t *testing.T, emailSuffix string) (ownerUserID, agentID, taskID string) {
	t.Helper()

	ownerUserID = createPlainMember(t, "che764-hermes-owner-"+emailSuffix+"@multica.test")

	// A dedicated workspace row using the exact literal ID the exception
	// hard-codes. Independent from testWorkspaceID — other tests' fixtures
	// are untouched.
	dbfx.Insert(t, "workspace", testutil.Cols{
		"id":           hermesExceptionWorkspaceID,
		"name":         "CHE-764 Hermes Exception Workspace",
		"slug":         "che764-hermes-ws-" + emailSuffix,
		"description":  "",
		"issue_prefix": "H764",
		"context":      "original workspace context",
	})
	dbfx.InsertNoID(t, "member", testutil.Cols{
		"workspace_id": hermesExceptionWorkspaceID,
		"user_id":      ownerUserID,
		"role":         "owner",
	}, "workspace_id = $1 AND user_id = $2", hermesExceptionWorkspaceID, ownerUserID)

	runtimeID := dbfx.Insert(t, "agent_runtime", testutil.Cols{
		"workspace_id": hermesExceptionWorkspaceID,
		"daemon_id":    nil,
		"name":         "CHE-764 Hermes Runtime",
		"runtime_mode": "cloud",
		"provider":     "handler_test_runtime",
		"status":       "online",
		"device_info":  "",
		"metadata":     testutil.Raw("'{}'::jsonb"),
		"last_seen_at": testutil.Raw("now()"),
		"visibility":   "private",
		"owner_id":     ownerUserID,
	})

	agentID = dbfx.Insert(t, "agent", testutil.Cols{
		"id":                   hermesExceptionAgentID,
		"workspace_id":         hermesExceptionWorkspaceID,
		"name":                 "c00-hermes-devops",
		"description":          "",
		"runtime_mode":         "cloud",
		"runtime_config":       testutil.Raw("'{}'::jsonb"),
		"runtime_id":           runtimeID,
		"visibility":           "private",
		"permission_mode":      "private",
		"max_concurrent_tasks": 1,
		"owner_id":             ownerUserID,
		"instructions":         "",
		"custom_env":           testutil.Raw("'{}'::jsonb"),
		"custom_args":          testutil.Raw("'[]'::jsonb"),
	})

	taskID = dbfx.Insert(t, "agent_task_queue", testutil.Cols{
		"agent_id":   agentID,
		"runtime_id": runtimeID,
		"status":     "running",
		"priority":   0,
		"started_at": testutil.Raw("now()"),
	})

	return ownerUserID, agentID, taskID
}

// hermesExceptionSquadFixture seeds a squad row whose id is EXACTLY
// hermesExceptionSquadID inside the given workspace, led by leaderID.
func hermesExceptionSquadFixture(t *testing.T, workspaceID, ownerUserID, leaderID string) {
	t.Helper()
	dbfx.Insert(t, "squad", testutil.Cols{
		"id":           hermesExceptionSquadID,
		"workspace_id": workspaceID,
		"name":         "dev team",
		"description":  "",
		"instructions": "original squad instructions",
		"leader_id":    leaderID,
		"creator_id":   ownerUserID,
	})
}

func hermesWorkspaceReq(hostAgentID, hostTaskID string, body map[string]any) *http.Request {
	req := withURLParam(newRequest(http.MethodPatch, "/api/workspaces/"+hermesExceptionWorkspaceID, body), "id", hermesExceptionWorkspaceID)
	return asAgentActor(req, hostAgentID, hostTaskID)
}

func hermesSquadReq(userID, hostAgentID, hostTaskID, squadID string, body map[string]any) *http.Request {
	req := squadReqWithParamsWorkspace(userID, "PATCH", "/api/squads", body, hermesExceptionWorkspaceID, map[string]string{"id": squadID})
	return asAgentActor(req, hostAgentID, hostTaskID)
}

// squadReqWithParamsWorkspace mirrors squadReqWithParams but lets the caller
// specify the workspaceId route param instead of hardcoding testWorkspaceID,
// since this file's squad fixture lives in hermesExceptionWorkspaceID.
func squadReqWithParamsWorkspace(userID, method, path string, body any, workspaceID string, params map[string]string) *http.Request {
	req := newRequestAs(userID, method, path, body)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("workspaceId", workspaceID)
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// ── Workspace context: positive ──────────────────────────────────────────

func TestHermesException_UpdateWorkspace_ExactAgentExactWorkspace_Succeeds(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	_, agentID, taskID := hermesExceptionFixture(t, "ws-ok")

	newContent := "line one\nline two — unicode: café ☕\nline three"
	before := testDigest("original workspace context")

	req := hermesWorkspaceReq(agentID, taskID, map[string]any{
		"context":                newContent,
		"expected_before_digest": before,
	})
	w := httptest.NewRecorder()
	testHandler.UpdateWorkspace(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	afterDigest, _ := resp["after_digest"].(string)
	if afterDigest != testDigest(newContent) {
		t.Errorf("after_digest = %q, want %q", afterDigest, testDigest(newContent))
	}

	var stored string
	dbfx.QueryRow(t, `SELECT context FROM workspace WHERE id = $1`, hermesExceptionWorkspaceID).Scan(&stored)
	if stored != newContent {
		t.Errorf("stored context = %q, want exact bytes %q", stored, newContent)
	}
}

// ── Workspace context: negative — different agent ────────────────────────

func TestHermesException_UpdateWorkspace_DifferentAgent_StillForbidden(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ownerUserID, _, _ := hermesExceptionFixture(t, "ws-other-agent")

	otherAgentID := dbfx.Insert(t, "agent", testutil.Cols{
		"workspace_id":         hermesExceptionWorkspaceID,
		"name":                 "not-hermes",
		"description":          "",
		"runtime_mode":         "cloud",
		"runtime_config":       testutil.Raw("'{}'::jsonb"),
		"visibility":           "private",
		"permission_mode":      "private",
		"max_concurrent_tasks": 1,
		"owner_id":             ownerUserID,
		"instructions":         "",
		"custom_env":           testutil.Raw("'{}'::jsonb"),
		"custom_args":          testutil.Raw("'[]'::jsonb"),
	})
	otherTaskID := createHandlerTestTaskForAgent(t, otherAgentID)

	req := hermesWorkspaceReq(otherAgentID, otherTaskID, map[string]any{
		"context":                "PWNED-BY-OTHER-AGENT",
		"expected_before_digest": testDigest("original workspace context"),
	})
	w := httptest.NewRecorder()
	testHandler.UpdateWorkspace(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-hermes agent, got %d: %s", w.Code, w.Body.String())
	}

	var stored string
	dbfx.QueryRow(t, `SELECT context FROM workspace WHERE id = $1`, hermesExceptionWorkspaceID).Scan(&stored)
	if stored != "original workspace context" {
		t.Errorf("context must remain untouched, got %q", stored)
	}
}

// ── Workspace context: negative — right agent, wrong workspace ──────────

func TestHermesException_UpdateWorkspace_RightAgentWrongWorkspace_StillForbidden(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	_, agentID, _ := hermesExceptionFixture(t, "ws-wrong-ws")

	// A second, ordinary workspace, NOT equal to hermesExceptionWorkspaceID.
	// resolveActor's in-process/mat_-fallback path (asAgentActor) requires
	// agent.WorkspaceID == the URL's workspace ID to resolve as "agent" at
	// all (see resolveActor, handler.go) — so hermesExceptionAgentID (bound
	// to hermesExceptionWorkspaceID) can NEVER resolve as "agent" against a
	// different workspace via that path; it always falls back to "member",
	// for any agent, which is a pre-existing property of resolveActor, not
	// something CHE-764 changes. The one path where a server-authenticated
	// agent identity DOES carry across to an arbitrary workspace without a
	// workspace-membership DB check is the real production path Hermes
	// actually uses: an `mat_` task token, which resolveActor trusts
	// directly via X-Actor-Source: task_token (see resolveActor's first
	// branch) without re-validating the agent/workspace pairing here. This
	// test simulates exactly that path — the one under which "right agent,
	// wrong workspace" is actually reachable — to prove the exception's
	// hermesExceptionMatchesWorkspaceContext workspace-ID check is what
	// rejects it, not an incidental actor-type fallback.
	otherWorkspaceID := dbfx.Insert(t, "workspace", testutil.Cols{
		"name":         "CHE-764 Other Workspace",
		"slug":         "che764-other-ws-wrong",
		"description":  "",
		"issue_prefix": "OTH1",
		"context":      "other workspace original context",
	})

	req := withURLParam(newRequest(http.MethodPatch, "/api/workspaces/"+otherWorkspaceID, map[string]any{
		"context":                "PWNED-WRONG-WORKSPACE",
		"expected_before_digest": testDigest("other workspace original context"),
	}), "id", otherWorkspaceID)
	req.Header.Set("X-Agent-ID", agentID)
	req.Header.Set("X-Actor-Source", "task_token")

	w := httptest.NewRecorder()
	testHandler.UpdateWorkspace(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for hermesExceptionAgentID (via task_token identity) on wrong workspace, got %d: %s", w.Code, w.Body.String())
	}

	var stored string
	dbfx.QueryRow(t, `SELECT context FROM workspace WHERE id = $1`, otherWorkspaceID).Scan(&stored)
	if stored != "other workspace original context" {
		t.Errorf("context must remain untouched, got %q", stored)
	}
}

// ── Workspace: right agent, right workspace, non-governed field ─────────
// Confirms no regression: fields outside the exception's scope behave
// exactly as before CHE-764 (agent actors may already write these).

func TestHermesException_UpdateWorkspace_OtherFieldUnaffected(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	_, agentID, taskID := hermesExceptionFixture(t, "ws-other-field")

	req := hermesWorkspaceReq(agentID, taskID, map[string]any{
		"description": "hermes agent writing an ordinary field",
	})
	w := httptest.NewRecorder()
	testHandler.UpdateWorkspace(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for non-governed field, got %d: %s", w.Code, w.Body.String())
	}
}

// ── Workspace context: negative — stale digest ───────────────────────────

func TestHermesException_UpdateWorkspace_StaleDigest_Rejected(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	_, agentID, taskID := hermesExceptionFixture(t, "ws-stale")

	req := hermesWorkspaceReq(agentID, taskID, map[string]any{
		"context":                "PWNED-VIA-STALE-DIGEST",
		"expected_before_digest": testDigest("this was never the live value"),
	})
	w := httptest.NewRecorder()
	testHandler.UpdateWorkspace(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for stale digest, got %d: %s", w.Code, w.Body.String())
	}

	var stored string
	dbfx.QueryRow(t, `SELECT context FROM workspace WHERE id = $1`, hermesExceptionWorkspaceID).Scan(&stored)
	if stored != "original workspace context" {
		t.Errorf("context must remain untouched after stale-digest rejection, got %q", stored)
	}
}

// ── Workspace context: negative — malformed digest ───────────────────────

func TestHermesException_UpdateWorkspace_MalformedDigest_Rejected(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	_, agentID, taskID := hermesExceptionFixture(t, "ws-malformed")

	cases := []struct {
		name   string
		digest string
	}{
		{"not hex", "not-a-valid-hex-digest-at-all"},
		{"too short", "abcd"},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := hermesWorkspaceReq(agentID, taskID, map[string]any{
				"context":                "irrelevant",
				"expected_before_digest": tc.digest,
			})
			w := httptest.NewRecorder()
			testHandler.UpdateWorkspace(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("digest %q: expected 400, got %d: %s", tc.digest, w.Code, w.Body.String())
			}
		})
	}

	var stored string
	dbfx.QueryRow(t, `SELECT context FROM workspace WHERE id = $1`, hermesExceptionWorkspaceID).Scan(&stored)
	if stored != "original workspace context" {
		t.Errorf("context must remain untouched after malformed-digest rejections, got %q", stored)
	}
}

// ── Squad instructions: positive, multiline/unicode roundtrip ───────────

func TestHermesException_UpdateSquad_ExactAgentExactSquad_MultilineUnicodeRoundtrip(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ownerUserID, agentID, taskID := hermesExceptionFixture(t, "sq-ok")
	leaderID := createHandlerTestAgent(t, "che764-hermes-squad-leader-ok", nil)
	hermesExceptionSquadFixture(t, hermesExceptionWorkspaceID, ownerUserID, leaderID)

	newInstructions := "Line one.\nLine two with unicode: 世界 🌍\n\nBlank line above.\tTabbed line.\n"
	before := testDigest("original squad instructions")

	req := hermesSquadReq(ownerUserID, agentID, taskID, hermesExceptionSquadID, map[string]any{
		"instructions":           newInstructions,
		"expected_before_digest": before,
	})
	w := httptest.NewRecorder()
	testHandler.UpdateSquad(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	afterDigest, _ := resp["after_digest"].(string)
	wantDigest := testDigest(newInstructions)
	if afterDigest != wantDigest {
		t.Errorf("after_digest = %q, want %q", afterDigest, wantDigest)
	}

	var stored string
	dbfx.QueryRow(t, `SELECT instructions FROM squad WHERE id = $1`, hermesExceptionSquadID).Scan(&stored)
	if stored != newInstructions {
		t.Errorf("stored instructions = %q, want exact bytes %q", stored, newInstructions)
	}
	if testDigest(stored) != wantDigest {
		t.Errorf("locally computed digest of stored bytes = %q, want %q", testDigest(stored), wantDigest)
	}
}

// ── Squad instructions: negative — different agent ───────────────────────

func TestHermesException_UpdateSquad_DifferentAgent_StillForbidden(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ownerUserID, _, _ := hermesExceptionFixture(t, "sq-other-agent")
	leaderID := createHandlerTestAgent(t, "che764-hermes-squad-leader-other", nil)
	hermesExceptionSquadFixture(t, hermesExceptionWorkspaceID, ownerUserID, leaderID)

	otherAgentID := createHandlerTestAgent(t, "che764-not-hermes-squad", nil)
	dbfx.Exec(t, `UPDATE agent SET owner_id = $1, workspace_id = $2 WHERE id = $3`, ownerUserID, hermesExceptionWorkspaceID, otherAgentID)
	otherTaskID := createHandlerTestTaskForAgent(t, otherAgentID)

	req := hermesSquadReq(ownerUserID, otherAgentID, otherTaskID, hermesExceptionSquadID, map[string]any{
		"instructions":           "PWNED-BY-OTHER-AGENT",
		"expected_before_digest": testDigest("original squad instructions"),
	})
	w := httptest.NewRecorder()
	testHandler.UpdateSquad(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-hermes agent, got %d: %s", w.Code, w.Body.String())
	}

	var stored string
	dbfx.QueryRow(t, `SELECT instructions FROM squad WHERE id = $1`, hermesExceptionSquadID).Scan(&stored)
	if stored != "original squad instructions" {
		t.Errorf("instructions must remain untouched, got %q", stored)
	}
}

// ── Squad instructions: negative — right agent, wrong squad ─────────────

func TestHermesException_UpdateSquad_RightAgentWrongSquad_StillForbidden(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ownerUserID, agentID, taskID := hermesExceptionFixture(t, "sq-wrong-squad")
	leaderID := createHandlerTestAgent(t, "che764-hermes-squad-leader-wrongsq", nil)
	otherSquad := createSquadInWorkspace(t, ownerUserID, hermesExceptionWorkspaceID, "Not The Dev Team Squad", leaderID)

	req := hermesSquadReq(ownerUserID, agentID, taskID, otherSquad, map[string]any{
		"instructions":           "PWNED-WRONG-SQUAD",
		"expected_before_digest": testDigest(""),
	})
	w := httptest.NewRecorder()
	testHandler.UpdateSquad(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for hermes agent on wrong squad, got %d: %s", w.Code, w.Body.String())
	}
}

// ── Squad: right agent, right squad, non-governed field ──────────────────

func TestHermesException_UpdateSquad_OtherFieldUnaffected(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ownerUserID, agentID, taskID := hermesExceptionFixture(t, "sq-other-field")
	leaderID := createHandlerTestAgent(t, "che764-hermes-squad-leader-otherfield", nil)
	hermesExceptionSquadFixture(t, hermesExceptionWorkspaceID, ownerUserID, leaderID)

	req := hermesSquadReq(ownerUserID, agentID, taskID, hermesExceptionSquadID, map[string]any{
		"name": "Renamed by hermes agent",
	})
	w := httptest.NewRecorder()
	testHandler.UpdateSquad(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for non-governed field, got %d: %s", w.Code, w.Body.String())
	}
}

// ── Squad instructions: negative — stale digest ──────────────────────────

func TestHermesException_UpdateSquad_StaleDigest_Rejected(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ownerUserID, agentID, taskID := hermesExceptionFixture(t, "sq-stale")
	leaderID := createHandlerTestAgent(t, "che764-hermes-squad-leader-stale", nil)
	hermesExceptionSquadFixture(t, hermesExceptionWorkspaceID, ownerUserID, leaderID)

	req := hermesSquadReq(ownerUserID, agentID, taskID, hermesExceptionSquadID, map[string]any{
		"instructions":           "PWNED-VIA-STALE-DIGEST",
		"expected_before_digest": testDigest("this was never the live value"),
	})
	w := httptest.NewRecorder()
	testHandler.UpdateSquad(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for stale digest, got %d: %s", w.Code, w.Body.String())
	}

	var stored string
	dbfx.QueryRow(t, `SELECT instructions FROM squad WHERE id = $1`, hermesExceptionSquadID).Scan(&stored)
	if stored != "original squad instructions" {
		t.Errorf("instructions must remain untouched after stale-digest rejection, got %q", stored)
	}
}

// ── Squad instructions: negative — malformed digest ──────────────────────

func TestHermesException_UpdateSquad_MalformedDigest_Rejected(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ownerUserID, agentID, taskID := hermesExceptionFixture(t, "sq-malformed")
	leaderID := createHandlerTestAgent(t, "che764-hermes-squad-leader-malformed", nil)
	hermesExceptionSquadFixture(t, hermesExceptionWorkspaceID, ownerUserID, leaderID)

	req := hermesSquadReq(ownerUserID, agentID, taskID, hermesExceptionSquadID, map[string]any{
		"instructions":           "irrelevant",
		"expected_before_digest": "zz-not-hex",
	})
	w := httptest.NewRecorder()
	testHandler.UpdateSquad(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed digest, got %d: %s", w.Code, w.Body.String())
	}

	var stored string
	dbfx.QueryRow(t, `SELECT instructions FROM squad WHERE id = $1`, hermesExceptionSquadID).Scan(&stored)
	if stored != "original squad instructions" {
		t.Errorf("instructions must remain untouched after malformed-digest rejection, got %q", stored)
	}
}

// ── Workspace context: negative — extra fields rejected ──────────────────
// Security review finding (CHE-764): the exception path is a dedicated
// single-field compare-and-swap; a request that also carries other fields
// must be rejected outright rather than silently dropping them.

func TestHermesException_UpdateWorkspace_ExtraFieldsRejected(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	_, agentID, taskID := hermesExceptionFixture(t, "ws-extra-fields")

	req := hermesWorkspaceReq(agentID, taskID, map[string]any{
		"context":                "irrelevant",
		"expected_before_digest": testDigest("original workspace context"),
		"name":                   "sneaking in a rename",
	})
	w := httptest.NewRecorder()
	testHandler.UpdateWorkspace(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when extra fields accompany the exception write, got %d: %s", w.Code, w.Body.String())
	}

	var storedContext, storedName string
	dbfx.QueryRow(t, `SELECT context, name FROM workspace WHERE id = $1`, hermesExceptionWorkspaceID).Scan(&storedContext, &storedName)
	if storedContext != "original workspace context" {
		t.Errorf("context must remain untouched, got %q", storedContext)
	}
	if storedName == "sneaking in a rename" {
		t.Errorf("name must not have been applied via the exception path")
	}
}

// ── Workspace context: positive — updated_at bumped, uppercase digest accepted ──

func TestHermesException_UpdateWorkspace_BumpsUpdatedAtAndAcceptsUppercaseDigest(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	_, agentID, taskID := hermesExceptionFixture(t, "ws-updated-at")

	var before time.Time
	dbfx.QueryRow(t, `SELECT updated_at FROM workspace WHERE id = $1`, hermesExceptionWorkspaceID).Scan(&before)

	uppercaseDigest := strings.ToUpper(testDigest("original workspace context"))
	req := hermesWorkspaceReq(agentID, taskID, map[string]any{
		"context":                "new content via uppercase digest",
		"expected_before_digest": uppercaseDigest,
	})
	w := httptest.NewRecorder()
	testHandler.UpdateWorkspace(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for well-formed uppercase digest, got %d: %s", w.Code, w.Body.String())
	}

	var after time.Time
	dbfx.QueryRow(t, `SELECT updated_at FROM workspace WHERE id = $1`, hermesExceptionWorkspaceID).Scan(&after)
	if !after.After(before) {
		t.Errorf("updated_at must advance after a successful exception write: before=%v after=%v", before, after)
	}
}

// ── Squad instructions: negative — extra fields rejected ─────────────────

func TestHermesException_UpdateSquad_ExtraFieldsRejected(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ownerUserID, agentID, taskID := hermesExceptionFixture(t, "sq-extra-fields")
	leaderID := createHandlerTestAgent(t, "che764-hermes-squad-leader-extrafields", nil)
	hermesExceptionSquadFixture(t, hermesExceptionWorkspaceID, ownerUserID, leaderID)

	req := hermesSquadReq(ownerUserID, agentID, taskID, hermesExceptionSquadID, map[string]any{
		"instructions":           "irrelevant",
		"expected_before_digest": testDigest("original squad instructions"),
		"name":                   "sneaking in a rename",
	})
	w := httptest.NewRecorder()
	testHandler.UpdateSquad(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when extra fields accompany the exception write, got %d: %s", w.Code, w.Body.String())
	}

	var storedInstructions, storedName string
	dbfx.QueryRow(t, `SELECT instructions, name FROM squad WHERE id = $1`, hermesExceptionSquadID).Scan(&storedInstructions, &storedName)
	if storedInstructions != "original squad instructions" {
		t.Errorf("instructions must remain untouched, got %q", storedInstructions)
	}
	if storedName == "sneaking in a rename" {
		t.Errorf("name must not have been applied via the exception path")
	}
}

// createSquadInWorkspace creates a squad directly (bypassing CreateSquad's
// HTTP path) inside the given workspace with the given creator/leader, for
// tests that need a squad ID guaranteed distinct from hermesExceptionSquadID.
func createSquadInWorkspace(t *testing.T, ownerUserID, workspaceID, name, leaderID string) string {
	t.Helper()
	squadID := dbfx.Insert(t, "squad", testutil.Cols{
		"workspace_id": workspaceID,
		"name":         name,
		"description":  "",
		"instructions": "",
		"leader_id":    leaderID,
		"creator_id":   ownerUserID,
	})
	dbfx.InsertNoID(t, "squad_member", testutil.Cols{
		"squad_id":    squadID,
		"member_type": "agent",
		"member_id":   leaderID,
		"role":        "leader",
	}, "squad_id = $1", squadID)
	return squadID
}
