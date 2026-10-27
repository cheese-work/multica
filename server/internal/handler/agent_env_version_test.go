package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func storedEnvVersion(t *testing.T, agentID string) int64 {
	t.Helper()
	var version int64
	if err := testPool.QueryRow(context.Background(), `SELECT custom_env_version FROM agent WHERE id = $1`, agentID).Scan(&version); err != nil {
		t.Fatalf("read custom_env_version: %v", err)
	}
	return version
}

// custom_env_version is an opaque counter: it moves on every env write and
// never derives from the env values (unlike envRevision, a plaintext hash).
func TestAgentEnv_VersionCountsEveryWrite(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, ownerID := agentEnvOwnerFixture(t, "env-version", "env-version@multica.test")
	if got := storedEnvVersion(t, agentID); got != 0 {
		t.Fatalf("fresh agent version = %d, want 0", got)
	}

	if w := patchAgentEnvAs(ownerID, agentID, map[string]any{"set": map[string]string{"EXTRA": "e"}}); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	if got := storedEnvVersion(t, agentID); got != 1 {
		t.Fatalf("version after PATCH = %d, want 1", got)
	}

	req := withURLParam(newRequestAs(ownerID, http.MethodPut, "/api/agents/"+agentID+"/env",
		map[string]any{"custom_env": map[string]string{"API_KEY": envSentinel}}), "id", agentID)
	w := httptest.NewRecorder()
	testHandler.UpdateAgentEnv(w, req)
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	if got := storedEnvVersion(t, agentID); got != 2 {
		t.Fatalf("version after PUT = %d, want 2", got)
	}
}
