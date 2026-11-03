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

// CHE-3296: the dedicated idle_started_guard field patches one settings key,
// validates it, and leaves every other key alone.
func TestWorkspaceIdleGuardField(t *testing.T) {
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

	patch := func(body map[string]any, want int) (settings map[string]any, errBody string) {
		t.Helper()
		request := withURLParam(newRequest(http.MethodPatch, "/api/workspaces/"+testWorkspaceID, body), "id", testWorkspaceID)
		rec := testutil.Call(t, testHandler.UpdateWorkspace, request).Want(want)
		stored, err := testHandler.Queries.GetWorkspace(ctx, parseUUID(testWorkspaceID))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(stored.Settings, &settings); err != nil {
			t.Fatal(err)
		}
		return settings, rec.Body.String()
	}
	keepsOthers := func(settings map[string]any) {
		t.Helper()
		for key, value := range others {
			if !reflect.DeepEqual(settings[key], value) {
				t.Errorf("settings[%q] = %v, want %v (full settings %v)", key, settings[key], value, settings)
			}
		}
	}

	settings, _ := patch(map[string]any{"idle_started_guard": map[string]any{"mode": "shadow", "after_hours": 6}}, http.StatusOK)
	if want := map[string]any{"mode": "shadow", "after_hours": float64(6)}; !reflect.DeepEqual(settings["idle_started_guard"], want) {
		t.Fatalf("idle_started_guard = %v, want %v", settings["idle_started_guard"], want)
	}
	keepsOthers(settings)

	// A partial object changes only the fields it names.
	settings, _ = patch(map[string]any{"idle_started_guard": map[string]any{"mode": "act"}}, http.StatusOK)
	if want := map[string]any{"mode": "act", "after_hours": float64(6)}; !reflect.DeepEqual(settings["idle_started_guard"], want) {
		t.Fatalf("partial patch: idle_started_guard = %v, want %v", settings["idle_started_guard"], want)
	}
	keepsOthers(settings)

	// A hand-set statuses list survives a mode change.
	dbfx.Exec(t, `UPDATE workspace SET settings = jsonb_set(settings, '{idle_started_guard,statuses}', '["in_progress"]') WHERE id = $1`, testWorkspaceID)
	settings, _ = patch(map[string]any{"idle_started_guard": map[string]any{"mode": "off"}}, http.StatusOK)
	if got := settings["idle_started_guard"].(map[string]any)["statuses"]; !reflect.DeepEqual(got, []any{"in_progress"}) {
		t.Fatalf("statuses lost on mode change: %v", settings["idle_started_guard"])
	}

	for _, accepted := range []float64{1, 72} {
		settings, _ = patch(map[string]any{"idle_started_guard": map[string]any{"after_hours": accepted}}, http.StatusOK)
		if got := settings["idle_started_guard"].(map[string]any)["after_hours"]; got != accepted {
			t.Fatalf("after_hours %v was not saved: %v", accepted, got)
		}
	}
	for _, invalid := range []any{0, 73, 2.5, "4", true, nil} {
		settings, body := patch(map[string]any{"idle_started_guard": map[string]any{"after_hours": invalid}}, http.StatusBadRequest)
		if !strings.Contains(body, "after_hours") {
			t.Errorf("after_hours %v: error %q does not name the field", invalid, body)
		}
		if got := settings["idle_started_guard"].(map[string]any)["after_hours"]; got != float64(72) {
			t.Fatalf("invalid after_hours %v changed the saved value: %v", invalid, settings)
		}
		keepsOthers(settings)
	}
	for _, invalid := range []any{"ON", "", 1, nil} {
		_, body := patch(map[string]any{"idle_started_guard": map[string]any{"mode": invalid}}, http.StatusBadRequest)
		if !strings.Contains(body, "mode") {
			t.Errorf("mode %v: error %q does not name the field", invalid, body)
		}
	}
	// Only mode and after_hours are writable here; statuses and any other
	// field would reach the guard's settings reader unchecked.
	for _, extra := range []string{"statuses", "extra"} {
		_, body := patch(map[string]any{"idle_started_guard": map[string]any{"mode": "act", extra: []any{"x"}}}, http.StatusBadRequest)
		if !strings.Contains(body, extra) {
			t.Errorf("unknown field %q: error %q does not name it", extra, body)
		}
	}
	for _, invalid := range []any{"on", 42, []any{}, nil} {
		patch(map[string]any{"idle_started_guard": invalid}, http.StatusBadRequest)
	}
}
