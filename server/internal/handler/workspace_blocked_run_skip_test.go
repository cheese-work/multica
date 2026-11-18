package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-6973: the blocked_run_skip field patches one settings key, accepts
// off|warn|block, and leaves every other key alone.
func TestWorkspaceBlockedRunSkipField(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setWorkspaceSettings(t, `{"compact_prompt":"Keep SHAs.","auto_close":{"mode":"block"}}`)
	patch := func(mode any, want int) string {
		t.Helper()
		request := withURLParam(newRequest(http.MethodPatch, "/api/workspaces/"+testWorkspaceID, map[string]any{"blocked_run_skip": map[string]any{"mode": mode}}), "id", testWorkspaceID)
		return testutil.Call(t, testHandler.UpdateWorkspace, request).Want(want).Body.String()
	}
	for _, mode := range []string{"warn", "block", "off"} {
		patch(mode, http.StatusOK)
		stored := storedSettings(t)
		if got := string(stored["blocked_run_skip"]); got != `{"mode": "`+mode+`"}` {
			t.Fatalf("blocked_run_skip = %s, want mode %s", got, mode)
		}
		if string(stored["auto_close"]) != `{"mode": "block"}` || string(stored["compact_prompt"]) != `"Keep SHAs."` {
			t.Fatalf("another settings key changed: %v", stored)
		}
	}
	for _, invalid := range []any{"ON", "", 1, nil} {
		if body := patch(invalid, http.StatusBadRequest); !strings.Contains(body, "mode") {
			t.Errorf("mode %v: error %q does not name the field", invalid, body)
		}
	}
}
