package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-9026: run_guard and run_budget each patch one settings key, accept
// off|warn|block, are owner only, and leave every other key alone.
func TestWorkspaceRunModeFields(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setWorkspaceSettings(t, `{"compact_prompt":"Keep SHAs.","auto_close":{"mode":"block"}}`)
	patch := func(field string, mode any, want int) string {
		t.Helper()
		request := withURLParam(newRequest(http.MethodPatch, "/api/workspaces/"+testWorkspaceID, map[string]any{field: map[string]any{"mode": mode}}), "id", testWorkspaceID)
		return testutil.Call(t, testHandler.UpdateWorkspace, request).Want(want).Body.String()
	}
	for _, field := range []string{"run_guard", "run_budget"} {
		for _, mode := range []string{"warn", "block", "off"} {
			patch(field, mode, http.StatusOK)
			stored := storedSettings(t)
			if got := string(stored[field]); got != `{"mode": "`+mode+`"}` {
				t.Fatalf("%s = %s, want mode %s", field, got, mode)
			}
			if string(stored["auto_close"]) != `{"mode": "block"}` || string(stored["compact_prompt"]) != `"Keep SHAs."` {
				t.Fatalf("another settings key changed: %v", stored)
			}
		}
		for _, invalid := range []any{"ON", "", 1, nil} {
			if body := patch(field, invalid, http.StatusBadRequest); !strings.Contains(body, field) {
				t.Errorf("%s mode %v: error %q does not name the field", field, invalid, body)
			}
		}
	}
	// Writing one mode leaves the other where it was.
	patch("run_guard", "off", http.StatusOK)
	patch("run_budget", "warn", http.StatusOK)
	if stored := storedSettings(t); string(stored["run_guard"]) != `{"mode": "off"}` || string(stored["run_budget"]) != `{"mode": "warn"}` {
		t.Fatalf("modes crossed: %v", stored)
	}
	// The owner rollout sends both modes in one request: both land, nothing else moves.
	setWorkspaceSettings(t, `{"compact_prompt":"Keep SHAs.","auto_close":{"mode":"block"}}`)
	request := withURLParam(newRequest(http.MethodPatch, "/api/workspaces/"+testWorkspaceID, map[string]any{
		"run_guard": map[string]any{"mode": "warn"}, "run_budget": map[string]any{"mode": "warn"},
	}), "id", testWorkspaceID)
	testutil.Call(t, testHandler.UpdateWorkspace, request).Want(http.StatusOK)
	stored := storedSettings(t)
	if string(stored["run_guard"]) != `{"mode": "warn"}` || string(stored["run_budget"]) != `{"mode": "warn"}` {
		t.Fatalf("both modes in one request: %v", stored)
	}
	if string(stored["auto_close"]) != `{"mode": "block"}` || string(stored["compact_prompt"]) != `"Keep SHAs."` {
		t.Fatalf("another settings key changed: %v", stored)
	}
	patch("run_guard", "off", http.StatusOK)
	patch("run_budget", "warn", http.StatusOK)
	// An admin is not the owner.
	setRuntimeTestMemberRole(t, testUserID, "admin")
	t.Cleanup(func() { setRuntimeTestMemberRole(t, testUserID, "owner") })
	for _, field := range []string{"run_guard", "run_budget"} {
		patch(field, "block", http.StatusForbidden)
	}
	both := withURLParam(newRequest(http.MethodPatch, "/api/workspaces/"+testWorkspaceID, map[string]any{
		"run_guard": map[string]any{"mode": "block"}, "run_budget": map[string]any{"mode": "block"},
	}), "id", testWorkspaceID)
	testutil.Call(t, testHandler.UpdateWorkspace, both).Want(http.StatusForbidden)
	if stored := storedSettings(t); string(stored["run_guard"]) != `{"mode": "off"}` || string(stored["run_budget"]) != `{"mode": "warn"}` {
		t.Fatalf("a refused write changed a mode: %v", stored)
	}
}
