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

// CHE-5987: the dedicated time_wakeup_skip field patches one settings key,
// accepts off|warn|block, is owner only, and leaves every other key alone.
func TestWorkspaceTimeWakeupSkipField(t *testing.T) {
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
	others := map[string]any{"compact_prompt": "Keep SHAs.", "idle_started_guard": map[string]any{"mode": "shadow"}}
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

	for _, mode := range []string{"warn", "block", "off"} {
		settings, _ := patch(map[string]any{"time_wakeup_skip": map[string]any{"mode": mode}}, http.StatusOK)
		if want := map[string]any{"mode": mode}; !reflect.DeepEqual(settings["time_wakeup_skip"], want) {
			t.Fatalf("time_wakeup_skip = %v, want %v", settings["time_wakeup_skip"], want)
		}
		keepsOthers(settings)
	}

	for _, invalid := range []any{"ON", "", 1, nil} {
		settings, body := patch(map[string]any{"time_wakeup_skip": map[string]any{"mode": invalid}}, http.StatusBadRequest)
		if !strings.Contains(body, "mode") {
			t.Errorf("mode %v: error %q does not name the field", invalid, body)
		}
		if got := settings["time_wakeup_skip"]; !reflect.DeepEqual(got, map[string]any{"mode": "off"}) {
			t.Fatalf("invalid mode %v changed the saved value: %v", invalid, got)
		}
		keepsOthers(settings)
	}
	for _, invalid := range []any{map[string]any{}, map[string]any{"mode": "warn", "extra": true}, "warn", 42, []any{}, nil} {
		patch(map[string]any{"time_wakeup_skip": invalid}, http.StatusBadRequest)
	}

	// An admin is not the owner.
	setRuntimeTestMemberRole(t, testUserID, "admin")
	t.Cleanup(func() { setRuntimeTestMemberRole(t, testUserID, "owner") })
	settings, _ := patch(map[string]any{"time_wakeup_skip": map[string]any{"mode": "block"}}, http.StatusForbidden)
	if got := settings["time_wakeup_skip"]; !reflect.DeepEqual(got, map[string]any{"mode": "off"}) {
		t.Fatalf("a refused write changed time_wakeup_skip: %v", got)
	}
}
