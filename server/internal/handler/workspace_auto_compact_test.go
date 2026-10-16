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

// CHE-1806: the dedicated top-level auto_compact field patches one settings
// key, validates it, and leaves every other key alone.
func TestWorkspaceAutoCompactField(t *testing.T) {
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

	settings, _ := patch(map[string]any{"auto_compact": map[string]any{"enabled": false, "threshold_pct": 70}}, http.StatusOK)
	if want := map[string]any{"enabled": false, "threshold_pct": float64(70)}; !reflect.DeepEqual(settings["auto_compact"], want) {
		t.Fatalf("auto_compact = %v, want %v", settings["auto_compact"], want)
	}
	keepsOthers(settings)

	// A partial object changes only the fields it names.
	settings, _ = patch(map[string]any{"auto_compact": map[string]any{"threshold_pct": 60}}, http.StatusOK)
	if want := map[string]any{"enabled": false, "threshold_pct": float64(60)}; !reflect.DeepEqual(settings["auto_compact"], want) {
		t.Fatalf("partial patch: auto_compact = %v, want %v", settings["auto_compact"], want)
	}
	keepsOthers(settings)

	// Bounds are 50 to 90 inclusive; each rejection names the field and
	// leaves the saved value as it was.
	for _, accepted := range []float64{50, 90} {
		settings, _ = patch(map[string]any{"auto_compact": map[string]any{"threshold_pct": accepted}}, http.StatusOK)
		if got := settings["auto_compact"].(map[string]any)["threshold_pct"]; got != accepted {
			t.Fatalf("threshold_pct %v was not saved: %v", accepted, got)
		}
	}
	for _, invalid := range []any{49, 91, 80.5, "80", true, nil, json.RawMessage("1e400")} {
		settings, body := patch(map[string]any{"auto_compact": map[string]any{"threshold_pct": invalid}}, http.StatusBadRequest)
		if !strings.Contains(body, "threshold_pct") {
			t.Errorf("threshold_pct %v: error %q does not name the field", invalid, body)
		}
		if got := settings["auto_compact"].(map[string]any)["threshold_pct"]; got != float64(90) {
			t.Fatalf("invalid threshold_pct %v changed the saved value: %v", invalid, settings)
		}
		keepsOthers(settings)
	}
	for _, invalid := range []any{"yes", 1, nil} {
		_, body := patch(map[string]any{"auto_compact": map[string]any{"enabled": invalid}}, http.StatusBadRequest)
		if !strings.Contains(body, "enabled") {
			t.Errorf("enabled %v: error %q does not name the field", invalid, body)
		}
	}
	for _, invalid := range []any{"on", 42, []any{}} {
		patch(map[string]any{"auto_compact": invalid}, http.StatusBadRequest)
	}

	// The settings blob path is validated the same way.
	patch(map[string]any{"settings": map[string]any{"auto_compact": map[string]any{"threshold_pct": 10}}}, http.StatusBadRequest)

	// null resets to the defaults by removing the key.
	settings, _ = patch(map[string]any{"auto_compact": nil}, http.StatusOK)
	if _, present := settings["auto_compact"]; present {
		t.Fatalf("null did not reset auto_compact: %v", settings)
	}
	keepsOthers(settings)
}

// A per-agent override lives in runtime_config.auto_compact and is validated
// like the workspace value; null inherits.
func TestAgentAutoCompactOverrideValidation(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "auto-compact-override-validation", []byte("[]"))
	update := func(config any, want int) {
		t.Helper()
		request := newRequest(http.MethodPatch, "/api/agents/"+agentID, map[string]any{"runtime_config": config})
		request = withChatTestWorkspaceCtx(t, withURLParam(request, "id", agentID))
		testutil.Call(t, testHandler.UpdateAgent, request).Want(want)
	}
	update(map[string]any{"auto_compact": map[string]any{"enabled": false}}, http.StatusOK)
	update(map[string]any{"auto_compact": map[string]any{"threshold_pct": 55}}, http.StatusOK)
	update(map[string]any{"auto_compact": nil}, http.StatusOK)
	update(map[string]any{"auto_compact": map[string]any{"threshold_pct": 95}}, http.StatusBadRequest)
	update(map[string]any{"auto_compact": map[string]any{"enabled": "no"}}, http.StatusBadRequest)
	update(map[string]any{"auto_compact": "off"}, http.StatusBadRequest)
}
