package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-6460 D1a: the dedicated decision_ask_park field sets one settings key,
// accepts always|on_wait, is owner only, and leaves every other key alone.
func TestWorkspaceDecisionAskParkField(t *testing.T) {
	ctx := context.Background()
	workspace, err := testHandler.Queries.GetWorkspace(ctx, parseUUID(testWorkspaceID))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = $1 WHERE id = $2`, workspace.Settings, testWorkspaceID)
	})
	seeded, _ := json.Marshal(map[string]any{"compact_prompt": "Keep SHAs."})
	dbfx.Exec(t, `UPDATE workspace SET settings = $1 WHERE id = $2`, seeded, testWorkspaceID)

	patch := func(body map[string]any, want int) (map[string]any, string) {
		t.Helper()
		request := withURLParam(newRequest(http.MethodPatch, "/api/workspaces/"+testWorkspaceID, body), "id", testWorkspaceID)
		rec := testutil.Call(t, testHandler.UpdateWorkspace, request).Want(want)
		stored, err := testHandler.Queries.GetWorkspace(ctx, parseUUID(testWorkspaceID))
		if err != nil {
			t.Fatal(err)
		}
		var settings map[string]any
		if err := json.Unmarshal(stored.Settings, &settings); err != nil {
			t.Fatal(err)
		}
		return settings, rec.Body.String()
	}

	for _, mode := range []string{"on_wait", "always"} {
		settings, _ := patch(map[string]any{"decision_ask_park": mode}, http.StatusOK)
		if settings["decision_ask_park"] != mode {
			t.Fatalf("decision_ask_park = %v, want %v", settings["decision_ask_park"], mode)
		}
		if settings["compact_prompt"] != "Keep SHAs." {
			t.Fatalf("compact_prompt = %v, want it kept", settings["compact_prompt"])
		}
	}
	for _, invalid := range []any{"ON_WAIT", "", "never", 1, nil, map[string]any{"mode": "on_wait"}} {
		settings, body := patch(map[string]any{"decision_ask_park": invalid}, http.StatusBadRequest)
		if !strings.Contains(body, "decision_ask_park") {
			t.Errorf("value %v: error %q does not name the field", invalid, body)
		}
		if got := settings["decision_ask_park"]; got != "always" {
			t.Fatalf("invalid value %v changed the saved value: %v", invalid, got)
		}
	}
	// The generic settings write validates the key too.
	if _, body := patch(map[string]any{"settings": map[string]any{"decision_ask_park": "bogus"}}, http.StatusBadRequest); !strings.Contains(body, "decision_ask_park") {
		t.Errorf("generic settings error %q does not name the key", body)
	}

	setRuntimeTestMemberRole(t, testUserID, "admin")
	t.Cleanup(func() { setRuntimeTestMemberRole(t, testUserID, "owner") })
	settings, _ := patch(map[string]any{"decision_ask_park": "on_wait"}, http.StatusForbidden)
	if got := settings["decision_ask_park"]; got != "always" {
		t.Fatalf("a refused write changed decision_ask_park: %v", got)
	}
}
