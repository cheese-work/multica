package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Phase A8a (CHE-6044): the env endpoints are a write-only facade. No response,
// log, audit row or broadcast carries a stored value. The planted value is
// synthetic, never a real credential.
const plantedEnvValue = "example-planted-7f3c9e"

var (
	facadeBusOnce sync.Once
	facadeBusMu   sync.Mutex
	facadeBusSeen []string
)

// captureAgentStatusPayloads records every agent:status payload, JSON-encoded.
func captureAgentStatusPayloads() func() []string {
	facadeBusOnce.Do(func() {
		testHandler.Bus.Subscribe(protocol.EventAgentStatus, func(e events.Event) {
			b, _ := json.Marshal(e.Payload)
			facadeBusMu.Lock()
			facadeBusSeen = append(facadeBusSeen, string(b))
			facadeBusMu.Unlock()
		})
	})
	facadeBusMu.Lock()
	start := len(facadeBusSeen)
	facadeBusMu.Unlock()
	return func() []string {
		facadeBusMu.Lock()
		defer facadeBusMu.Unlock()
		return append([]string(nil), facadeBusSeen[start:]...)
	}
}

func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func plantedEnvFixture(t *testing.T, name string) (agentID, ownerID string) {
	t.Helper()
	agentID, ownerID = agentEnvOwnerFixture(t, name, name+"@multica.test")
	if _, err := testPool.Exec(context.Background(),
		`UPDATE agent SET custom_env = jsonb_build_object('PLANTED_KEY', $1::text) WHERE id = $2`, plantedEnvValue, agentID); err != nil {
		t.Fatalf("plant env: %v", err)
	}
	return agentID, ownerID
}

func getAgentEnvAs(userID, agentID string) *httptest.ResponseRecorder {
	req := withURLParam(newRequestAs(userID, http.MethodGet, "/api/agents/"+agentID+"/env", nil), "id", agentID)
	w := httptest.NewRecorder()
	testHandler.GetAgentEnv(w, req)
	return w
}

func putAgentEnvAs(userID, agentID string, body any) *httptest.ResponseRecorder {
	req := withURLParam(newRequestAs(userID, http.MethodPut, "/api/agents/"+agentID+"/env", body), "id", agentID)
	w := httptest.NewRecorder()
	testHandler.UpdateAgentEnv(w, req)
	return w
}

func assertNoPlanted(t *testing.T, what, got string) {
	t.Helper()
	if strings.Contains(got, plantedEnvValue) {
		t.Errorf("%s leaks the planted value: %s", what, got)
	}
}

type facadeEnvBody struct {
	AgentID   string            `json:"agent_id"`
	CustomEnv map[string]string `json:"custom_env"`
	Keys      []struct {
		Key    string `json:"key"`
		Source string `json:"source"`
	} `json:"keys"`
	Revision string `json:"revision"`
}

func decodeFacade(t *testing.T, w *httptest.ResponseRecorder) facadeEnvBody {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var b facadeEnvBody
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return b
}

// Checks 1 and 2 (GET): masked body, keys metadata, no-store, no reveal audit.
func TestAgentEnvFacade_GetIsMetadataOnly(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, ownerID := plantedEnvFixture(t, "env-facade-get")

	w := getAgentEnvAs(ownerID, agentID)
	assertNoPlanted(t, "GET body", w.Body.String())
	body := decodeFacade(t, w)
	if body.CustomEnv["PLANTED_KEY"] != envSentinel || len(body.CustomEnv) != 1 {
		t.Fatalf("custom_env must be masked: %v", body.CustomEnv)
	}
	if len(body.Keys) != 1 || body.Keys[0].Key != "PLANTED_KEY" || body.Keys[0].Source != "legacy" {
		t.Fatalf("keys = %+v, want [{PLANTED_KEY legacy}]", body.Keys)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM activity_log WHERE action = 'agent_env_revealed' AND details->>'agent_id' = $1`, agentID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("GET wrote %d agent_env_revealed rows; the action is retired", n)
	}
}

// Check 1 (PUT, PATCH) and check 2 (revision token).
func TestAgentEnvFacade_WritesReturnMaskedAndCounterRevision(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, ownerID := plantedEnvFixture(t, "env-facade-write")
	version := storedEnvVersion(t, agentID)

	// PUT carrying the sentinel keeps the stored value but never echoes it.
	w := putAgentEnvAs(ownerID, agentID, map[string]any{"custom_env": map[string]string{"PLANTED_KEY": envSentinel}})
	assertNoPlanted(t, "PUT body", w.Body.String())
	put := decodeFacade(t, w)
	if put.CustomEnv["PLANTED_KEY"] != envSentinel {
		t.Fatalf("PUT custom_env must be masked: %v", put.CustomEnv)
	}
	if got := storedAgentEnv(t, agentID)["PLANTED_KEY"]; got != plantedEnvValue {
		t.Fatalf("sentinel must keep the stored value, got %q", got)
	}
	if want := fmt.Sprintf("v%d", version+1); put.Revision != want {
		t.Fatalf("PUT revision = %q, want %q", put.Revision, want)
	}

	w = patchAgentEnvAs(ownerID, agentID, map[string]any{"set": map[string]string{"OTHER": "x"}})
	assertNoPlanted(t, "PATCH body", w.Body.String())
	patch := decodeFacade(t, w)
	if want := fmt.Sprintf("v%d", version+2); patch.Revision != want {
		t.Fatalf("PATCH revision = %q, want %q (changes on every write)", patch.Revision, want)
	}

	// Never a hash of the plaintext map.
	plain, _ := json.Marshal(storedAgentEnv(t, agentID))
	sum := sha256.Sum256(plain)
	if patch.Revision == hex.EncodeToString(sum[:]) {
		t.Fatal("revision is the SHA-256 of the plaintext map")
	}

	// if_revision compares the counter token.
	if w := patchAgentEnvAs(ownerID, agentID, map[string]any{"set": map[string]string{"A": "1"}, "if_revision": put.Revision}); w.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale counter revision: %d %s", w.Code, w.Body.String())
	}
	if w := patchAgentEnvAs(ownerID, agentID, map[string]any{"set": map[string]string{"A": "1"}, "if_revision": patch.Revision}); w.Code != http.StatusOK {
		t.Fatalf("current counter revision: %d %s", w.Code, w.Body.String())
	}
}

// Checks 4 and 6: error bodies and slog output carry no planted value.
func TestAgentEnvFacade_ErrorPathsAndLogsHoldNoPlantedValue(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	logs := captureSlog(t)
	agentID, ownerID := plantedEnvFixture(t, "env-facade-errors")
	strangerID := createPermissionTestMember(t, "env-facade-stranger@multica.test")

	bad := func(method string, h func(http.ResponseWriter, *http.Request), user string, body any, want int) {
		t.Helper()
		req := withURLParam(newRequestAs(user, method, "/api/agents/"+agentID+"/env", body), "id", agentID)
		w := httptest.NewRecorder()
		h(w, req)
		if w.Code != want {
			t.Fatalf("%s: status %d, want %d: %s", method, w.Code, want, w.Body.String())
		}
		assertNoPlanted(t, fmt.Sprintf("%s %d body", method, want), w.Body.String())
	}
	planted := map[string]string{"PLANTED_KEY": plantedEnvValue}

	// 403 on all three routes.
	bad(http.MethodGet, testHandler.GetAgentEnv, strangerID, nil, http.StatusForbidden)
	bad(http.MethodPut, testHandler.UpdateAgentEnv, strangerID, map[string]any{"custom_env": planted}, http.StatusForbidden)
	bad(http.MethodPatch, testHandler.PatchAgentEnv, strangerID, map[string]any{"set": planted}, http.StatusForbidden)
	// 400: malformed body, empty patch, sentinel in set, empty key.
	bad(http.MethodPut, testHandler.UpdateAgentEnv, ownerID, `{"custom_env": `+plantedEnvValue, http.StatusBadRequest)
	bad(http.MethodPatch, testHandler.PatchAgentEnv, ownerID, `{"set": `+plantedEnvValue, http.StatusBadRequest)
	bad(http.MethodPatch, testHandler.PatchAgentEnv, ownerID, map[string]any{"set": map[string]string{"": plantedEnvValue}}, http.StatusBadRequest)
	bad(http.MethodPatch, testHandler.PatchAgentEnv, ownerID, map[string]any{"set": map[string]string{"K": envSentinel}}, http.StatusBadRequest)
	// 412: stale revision.
	bad(http.MethodPatch, testHandler.PatchAgentEnv, ownerID,
		map[string]any{"set": planted, "if_revision": "v-stale-" + plantedEnvValue}, http.StatusPreconditionFailed)
	// 500 is covered by TestAgentEnvFacade_InternalErrorBodiesAndLogsHoldNoPlantedValue.
	// 409, 413: no handler path produces them today (no conflict state, no body cap).

	// A successful write adds to the same log capture.
	if w := putAgentEnvAs(ownerID, agentID, map[string]any{"custom_env": planted}); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	assertNoPlanted(t, "slog output", logs.String())
}

// Check 4 (500) and check 6 (failure-path logs): a transaction-start failure and
// an audit-write failure, each on PUT and PATCH with the planted value in the
// request body, leave the 500 body and the captured slog output free of it.
func TestAgentEnvFacade_InternalErrorBodiesAndLogsHoldNoPlantedValue(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	logs := captureSlog(t)
	agentID, ownerID := plantedEnvFixture(t, "env-facade-500")
	planted := map[string]string{"PLANTED_KEY": plantedEnvValue, "NEW_KEY": plantedEnvValue}

	call := func(h *Handler, method string, body any) *httptest.ResponseRecorder {
		req := withURLParam(newRequestAs(ownerID, method, "/api/agents/"+agentID+"/env", body), "id", agentID)
		w := httptest.NewRecorder()
		if method == http.MethodPut {
			h.UpdateAgentEnv(w, req)
		} else {
			h.PatchAgentEnv(w, req)
		}
		return w
	}
	bodies := map[string]any{
		http.MethodPut:   map[string]any{"custom_env": planted},
		http.MethodPatch: map[string]any{"set": planted},
	}
	expect500 := func(what string, w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("%s: status %d, want 500: %s", what, w.Code, w.Body.String())
		}
		assertNoPlanted(t, what+" 500 body", w.Body.String())
	}

	// Transaction-start failure.
	for _, method := range []string{http.MethodPut, http.MethodPatch} {
		failing := *testHandler
		failing.TxStarter = &failNthBegin{delegate: testPool, failAt: 1}
		expect500(method+" begin-tx failure", call(&failing, method, bodies[method]))
	}

	// Audit-write failure (activity_log insert raises), scoped to this agent.
	for _, stmt := range []string{
		`CREATE OR REPLACE FUNCTION che6044_fail_audit() RETURNS trigger AS $$
		 BEGIN
		   IF NEW.action = 'agent_env_updated' AND NEW.details->>'agent_id' = '` + agentID + `' THEN
		     RAISE EXCEPTION 'synthetic audit outage';
		   END IF;
		   RETURN NEW;
		 END $$ LANGUAGE plpgsql`,
		`CREATE TRIGGER che6044_fail_audit BEFORE INSERT ON activity_log FOR EACH ROW EXECUTE FUNCTION che6044_fail_audit()`,
	} {
		if _, err := testPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("install failing audit trigger: %v", err)
		}
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DROP TRIGGER IF EXISTS che6044_fail_audit ON activity_log`)
		testPool.Exec(ctx, `DROP FUNCTION IF EXISTS che6044_fail_audit()`)
	})
	for _, method := range []string{http.MethodPut, http.MethodPatch} {
		expect500(method+" audit failure", call(testHandler, method, bodies[method]))
	}
	if got := storedAgentEnv(t, agentID); len(got) != 1 || got["PLANTED_KEY"] != plantedEnvValue {
		t.Fatalf("failed writes changed the stored env: %v", got)
	}

	if logs.Len() == 0 {
		t.Fatal("expected failure-path log lines; the log capture saw nothing")
	}
	assertNoPlanted(t, "slog output", logs.String())
}

// Checks 5 and 7: activity/audit rows hold key names only; agent:status carries no value.
func TestAgentEnvFacade_ActivityAuditAndBroadcastHoldNoPlantedValue(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	seen := captureAgentStatusPayloads()
	agentID, ownerID := plantedEnvFixture(t, "env-facade-audit")

	if w := putAgentEnvAs(ownerID, agentID, map[string]any{"custom_env": map[string]string{"PLANTED_KEY": plantedEnvValue + "-b"}}); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	if w := patchAgentEnvAs(ownerID, agentID, map[string]any{"set": map[string]string{"N": plantedEnvValue}}); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}

	var activity, audit int
	var rows string
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*), coalesce(string_agg(details::text, ' '), '') FROM activity_log WHERE details->>'agent_id' = $1`, agentID).Scan(&activity, &rows); err != nil {
		t.Fatal(err)
	}
	if activity < 2 {
		t.Fatalf("expected activity rows, got %d", activity)
	}
	assertNoPlanted(t, "activity.details", rows)
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*), coalesce(string_agg("before"::text || "after"::text, ' '), '') FROM workspace_audit_event WHERE kind = 'agent.env.update' AND target_id = $1`, agentID).Scan(&audit, &rows); err != nil {
		t.Fatal(err)
	}
	if audit < 2 {
		t.Fatalf("expected agent.env.update audit rows, got %d", audit)
	}
	assertNoPlanted(t, "agent.env.update audit row", rows)

	events := seen()
	if len(events) < 2 {
		t.Fatalf("expected agent:status events, got %d", len(events))
	}
	for _, e := range events {
		assertNoPlanted(t, "agent:status event", e)
	}
}

// Check 8: internal delivery is kept; the claim response still carries the value.
func TestAgentEnvFacade_ClaimStillCarriesValue(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	runtimeID := createClaimReclaimRuntime(t, ctx, "env-facade claim runtime")
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "env-facade claim agent")
	if _, err := testPool.Exec(ctx, `UPDATE agent SET custom_env = jsonb_build_object('PLANTED_KEY', $1::text) WHERE id = $2`, plantedEnvValue, agentID); err != nil {
		t.Fatal(err)
	}
	var taskID string
	if err := testPool.QueryRow(ctx, `INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority) VALUES ($1, $2, $3, 'queued', 0) RETURNING id`,
		agentID, runtimeID, issueID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID) })

	w := httptest.NewRecorder()
	req := withURLParam(newOwnerDaemonRequest("POST", "/api/daemon/runtimes/"+runtimeID+"/tasks/claim", nil), "runtimeId", runtimeID)
	testHandler.ClaimTaskByRuntime(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("claim: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Task *AgentTaskResponse `json:"task"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.Task == nil || resp.Task.Agent == nil {
		t.Fatalf("claim decode: %v %s", err, w.Body.String())
	}
	if got := resp.Task.Agent.CustomEnv["PLANTED_KEY"]; got != plantedEnvValue {
		t.Fatalf("claim custom_env = %v, want planted value delivered", resp.Task.Agent.CustomEnv)
	}
}

// Renaming a masked key in the editor sends {NEW: "****"} and omits OLD. The
// server cannot move a value it never shows, so it must reject the write
// instead of silently deleting the stored secret (review P1).
func TestAgentEnvFacade_RenamedMaskedKeyIsRejectedAndKeepsSecret(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, ownerID := plantedEnvFixture(t, "env-facade-rename")
	version := storedEnvVersion(t, agentID)

	w := putAgentEnvAs(ownerID, agentID, map[string]any{"custom_env": map[string]string{"RENAMED_KEY": envSentinel}})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("renamed masked key: %d %s", w.Code, w.Body.String())
	}
	assertNoPlanted(t, "rename error body", w.Body.String())
	if got := storedAgentEnv(t, agentID); len(got) != 1 || got["PLANTED_KEY"] != plantedEnvValue {
		t.Fatalf("rejected rename changed the stored env: %v", got)
	}
	if v := storedEnvVersion(t, agentID); v != version {
		t.Fatalf("rejected rename bumped the revision counter: %d -> %d", version, v)
	}

	// A rename that supplies a real value is a normal remove + add.
	w = putAgentEnvAs(ownerID, agentID, map[string]any{"custom_env": map[string]string{"RENAMED_KEY": "new-value"}})
	if w.Code != http.StatusOK {
		t.Fatalf("rename with a new value: %d %s", w.Code, w.Body.String())
	}
}
