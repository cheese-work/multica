package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func patchAgentEnvAs(userID, agentID string, body any) *httptest.ResponseRecorder {
	req := withURLParam(newRequestAs(userID, http.MethodPatch, "/api/agents/"+agentID+"/env", body), "id", agentID)
	w := httptest.NewRecorder()
	testHandler.PatchAgentEnv(w, req)
	return w
}

func storedAgentEnv(t *testing.T, agentID string) map[string]string {
	t.Helper()
	var raw string
	if err := testPool.QueryRow(context.Background(), `SELECT custom_env::text FROM agent WHERE id = $1`, agentID).Scan(&raw); err != nil {
		t.Fatalf("read custom_env: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("decode custom_env: %v", err)
	}
	return got
}

// Concurrent additions on distinct keys must all survive, alongside the
// untouched seed key. A read-before-lock merge loses all but one.
func TestPatchAgentEnv_ConcurrentAdditionsPreserved(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, ownerID := agentEnvOwnerFixture(t, "env-patch-concurrent", "env-patch-concurrent@multica.test")

	const writers = 16
	var wg sync.WaitGroup
	codes := make([]int, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = patchAgentEnvAs(ownerID, agentID, map[string]any{
				"set": map[string]string{fmt.Sprintf("KEY_%02d", i): fmt.Sprintf("v%d", i)},
			}).Code
		}(i)
	}
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("writer %d: status %d", i, c)
		}
	}
	got := storedAgentEnv(t, agentID)
	if len(got) != writers+1 || got["API_KEY"] != "secret-value" {
		t.Fatalf("lost a concurrent addition or the seed key: %v", got)
	}
}

// Compare-and-set: N writers all holding the same revision, exactly one wins;
// the rest get 412 and write nothing.
func TestPatchAgentEnv_StaleRevisionRejected(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, ownerID := agentEnvOwnerFixture(t, "env-patch-cas", "env-patch-cas@multica.test")
	rev := envRevision(map[string]string{"API_KEY": "secret-value"})

	const writers = 8
	var wg sync.WaitGroup
	codes := make([]int, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = patchAgentEnvAs(ownerID, agentID, map[string]any{
				"set":         map[string]string{fmt.Sprintf("CAS_%d", i): "x"},
				"if_revision": rev,
			}).Code
		}(i)
	}
	wg.Wait()

	wins, stale := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			wins++
		case http.StatusPreconditionFailed:
			stale++
		default:
			t.Fatalf("unexpected status %d", c)
		}
	}
	if wins != 1 || stale != writers-1 {
		t.Fatalf("wins=%d stale=%d; want exactly one winner", wins, stale)
	}
	if got := storedAgentEnv(t, agentID); len(got) != 2 {
		t.Fatalf("losers wrote: %v", got)
	}

	// A fresh revision from the response applies.
	w := patchAgentEnvAs(ownerID, agentID, map[string]any{"unset": []string{"API_KEY"}})
	var resp AgentEnvResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || w.Code != http.StatusOK {
		t.Fatalf("unset: %d %s", w.Code, w.Body.String())
	}
	w = patchAgentEnvAs(ownerID, agentID, map[string]any{
		"set": map[string]string{"AFTER": "y"}, "if_revision": resp.Revision,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("patch with current revision: %d %s", w.Code, w.Body.String())
	}
	if _, ok := storedAgentEnv(t, agentID)["API_KEY"]; ok {
		t.Fatal("unset key still stored")
	}
}

func TestPatchAgentEnv_MasksValuesAndAuditsKeysOnly(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, ownerID := agentEnvOwnerFixture(t, "env-patch-mask", "env-patch-mask@multica.test")

	w := patchAgentEnvAs(ownerID, agentID, map[string]any{"set": map[string]string{"NEW_KEY": "brand-new-secret"}})
	if w.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "brand-new-secret") || strings.Contains(w.Body.String(), "secret-value") {
		t.Fatalf("response leaked a value: %s", w.Body.String())
	}
	var details string
	if err := testPool.QueryRow(context.Background(), `
		SELECT details::text FROM activity_log
		WHERE action = 'agent_env_updated' AND details->>'agent_id' = $1
		ORDER BY created_at DESC LIMIT 1`, agentID).Scan(&details); err != nil {
		t.Fatalf("audit row: %v", err)
	}
	if strings.Contains(details, "brand-new-secret") || !strings.Contains(details, "NEW_KEY") || !strings.Contains(details, `"patch"`) {
		t.Fatalf("audit details: %s", details)
	}
}

func TestPatchAgentEnv_RejectsBadRequests(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, ownerID := agentEnvOwnerFixture(t, "env-patch-validate", "env-patch-validate@multica.test")
	cases := map[string]any{
		"empty":         map[string]any{},
		"sentinel":      map[string]any{"set": map[string]string{"K": envSentinel}},
		"empty key":     map[string]any{"set": map[string]string{"": "v"}},
		"set and unset": map[string]any{"set": map[string]string{"K": "v"}, "unset": []string{"K"}},
	}
	for name, body := range cases {
		if w := patchAgentEnvAs(ownerID, agentID, body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, w.Code)
		}
	}
	if got := storedAgentEnv(t, agentID); len(got) != 1 || got["API_KEY"] != "secret-value" {
		t.Fatalf("rejected request changed env: %v", got)
	}
}

func TestPatchAgentEnv_AuthorizationBoundaries(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID, ownerID := agentEnvOwnerFixture(t, "env-patch-authz", "env-patch-authz@multica.test")
	strangerID := createPermissionTestMember(t, "env-patch-stranger@multica.test")

	if w := patchAgentEnvAs(strangerID, agentID, map[string]any{"set": map[string]string{"X": "y"}}); w.Code != http.StatusForbidden {
		t.Fatalf("unrelated member: %d", w.Code)
	}

	// Agent actor on behalf of the owner is still rejected.
	hostID := createHandlerTestAgent(t, "env-patch-host", nil)
	if _, err := testPool.Exec(ctx, `UPDATE agent SET owner_id = $1 WHERE id = $2`, ownerID, hostID); err != nil {
		t.Fatal(err)
	}
	req := withURLParam(newRequestAs(ownerID, http.MethodPatch, "/api/agents/"+agentID+"/env",
		map[string]any{"set": map[string]string{"X": "y"}}), "id", agentID)
	req.Header.Set("X-Agent-ID", hostID)
	req.Header.Set("X-Task-ID", createHandlerTestTaskForAgent(t, hostID))
	w := httptest.NewRecorder()
	testHandler.PatchAgentEnv(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("agent actor: %d", w.Code)
	}
	if got := storedAgentEnv(t, agentID); len(got) != 1 {
		t.Fatalf("denied request changed env: %v", got)
	}
}

// An audit-write failure must leave the env untouched.
func TestPatchAgentEnv_AuditFailureRollsBack(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID, ownerID := agentEnvOwnerFixture(t, "env-patch-audit", "env-patch-audit@multica.test")

	// Scoped to this agent so parallel tests are unaffected.
	for _, stmt := range []string{
		`CREATE OR REPLACE FUNCTION che1039_fail_audit() RETURNS trigger AS $$
		 BEGIN
		   IF NEW.action = 'agent_env_updated' AND NEW.details->>'agent_id' = '` + agentID + `' THEN
		     RAISE EXCEPTION 'synthetic audit outage';
		   END IF;
		   RETURN NEW;
		 END $$ LANGUAGE plpgsql`,
		`CREATE TRIGGER che1039_fail_audit BEFORE INSERT ON activity_log FOR EACH ROW EXECUTE FUNCTION che1039_fail_audit()`,
	} {
		if _, err := testPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("install failing audit trigger: %v", err)
		}
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DROP TRIGGER IF EXISTS che1039_fail_audit ON activity_log`)
		testPool.Exec(ctx, `DROP FUNCTION IF EXISTS che1039_fail_audit()`)
	})

	w := patchAgentEnvAs(ownerID, agentID, map[string]any{"set": map[string]string{"LOST": "v"}, "unset": []string{"API_KEY"}})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500: %s", w.Code, w.Body.String())
	}
	if got := storedAgentEnv(t, agentID); len(got) != 1 || got["API_KEY"] != "secret-value" {
		t.Fatalf("audit failure left a partial write: %v", got)
	}
}

// PUT keeps its wholesale contract after the lock change: omitted keys are
// removed, **** preserves a stored value.
func TestUpdateAgentEnv_FullMapContractUnchanged(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, ownerID := agentEnvOwnerFixture(t, "env-put-contract", "env-put-contract@multica.test")
	if w := patchAgentEnvAs(ownerID, agentID, map[string]any{"set": map[string]string{"EXTRA": "e"}}); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	req := withURLParam(newRequestAs(ownerID, http.MethodPut, "/api/agents/"+agentID+"/env",
		map[string]any{"custom_env": map[string]string{"API_KEY": envSentinel}}), "id", agentID)
	w := httptest.NewRecorder()
	testHandler.UpdateAgentEnv(w, req)
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	if got := storedAgentEnv(t, agentID); len(got) != 1 || got["API_KEY"] != "secret-value" {
		t.Fatalf("PUT contract changed: %v", got)
	}
}
