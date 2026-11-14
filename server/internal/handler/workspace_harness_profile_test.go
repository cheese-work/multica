package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-3706: the dedicated harness_profile field sets or removes one settings
// key, validates it, and leaves every other key alone.
func TestWorkspaceHarnessProfileField(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	workspace, err := testHandler.Queries.GetWorkspace(ctx, parseUUID(testWorkspaceID))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = $1 WHERE id = $2`, workspace.Settings, testWorkspaceID)
	})
	others := map[string]any{"compact_prompt": "Keep SHAs.", "nested": map[string]any{"keep": "me"}}
	seeded, _ := json.Marshal(others)
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
	keepsOthers := func(settings map[string]any) {
		t.Helper()
		for key, value := range others {
			if !reflect.DeepEqual(settings[key], value) {
				t.Errorf("settings[%q] = %v, want %v", key, settings[key], value)
			}
		}
	}

	settings, _ := patch(map[string]any{"harness_profile": "managed"}, http.StatusOK)
	if settings["harness_profile"] != "managed" {
		t.Fatalf("harness_profile = %v, want managed", settings["harness_profile"])
	}
	keepsOthers(settings)

	for _, invalid := range []any{"managed_sandbox", "", 7, true} {
		settings, body := patch(map[string]any{"harness_profile": invalid}, http.StatusBadRequest)
		if !strings.Contains(body, "harness_profile") {
			t.Errorf("%v: error %q does not name the field", invalid, body)
		}
		if settings["harness_profile"] != "managed" {
			t.Errorf("%v: saved value changed to %v", invalid, settings["harness_profile"])
		}
	}

	settings, _ = patch(map[string]any{"harness_profile": nil}, http.StatusOK)
	if _, present := settings["harness_profile"]; present {
		t.Fatalf("null did not remove harness_profile: %v", settings)
	}
	keepsOthers(settings)
}
