package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// CHE-5090 (CHE-4678 S9 P1): execution_mode "script" stores a script name and
// an args list in script_config. Never a shell string.

func postScriptAutopilot(t *testing.T, title string, mode string, scriptConfig any) *httptest.ResponseRecorder {
	t.Helper()
	agentID := createHandlerTestAgent(t, title+"-agent", nil)
	body := map[string]any{
		"title":          title,
		"assignee_id":    agentID,
		"execution_mode": mode,
	}
	if scriptConfig != nil {
		body["script_config"] = scriptConfig
	}
	w := httptest.NewRecorder()
	testHandler.CreateAutopilot(w, newRequest("POST", "/api/autopilots?workspace_id="+testWorkspaceID, body))
	if w.Code == http.StatusCreated {
		var ap AutopilotResponse
		if err := json.Unmarshal(w.Body.Bytes(), &ap); err == nil {
			t.Cleanup(func() {
				testPool.Exec(context.Background(), `DELETE FROM autopilot WHERE id = $1`, ap.ID)
			})
		}
	}
	return w
}

func TestCreateAutopilot_ScriptMode_AcceptsValidConfig(t *testing.T) {
	w := postScriptAutopilot(t, "script-ok", "script", map[string]any{
		"name": "sweep-reconcile",
		"args": []string{"--dry-run", "--limit=5"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body.String())
	}
	var ap AutopilotResponse
	if err := json.Unmarshal(w.Body.Bytes(), &ap); err != nil {
		t.Fatal(err)
	}
	if ap.ExecutionMode != "script" {
		t.Fatalf("execution_mode = %q, want script", ap.ExecutionMode)
	}
	var cfg struct {
		Name string   `json:"name"`
		Args []string `json:"args"`
	}
	if err := json.Unmarshal(ap.ScriptConfig, &cfg); err != nil {
		t.Fatalf("script_config not returned: %v", err)
	}
	if cfg.Name != "sweep-reconcile" || len(cfg.Args) != 2 {
		t.Fatalf("script_config = %+v", cfg)
	}
}

func TestCreateAutopilot_ScriptMode_AcceptsEmptyArgs(t *testing.T) {
	w := postScriptAutopilot(t, "script-noargs", "script", map[string]any{"name": "sweep-reconcile"})
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateAutopilot_ScriptMode_RejectsInvalidConfig(t *testing.T) {
	longArg := strings.Repeat("a", scriptMaxArgLen+1)
	tooMany := make([]string, scriptMaxArgs+1)
	for i := range tooMany {
		tooMany[i] = "x"
	}
	cases := []struct {
		name string
		cfg  any
	}{
		{"missing config", nil},
		{"config is a shell string", "rm -rf / && echo hi"},
		{"empty name", map[string]any{"name": ""}},
		{"name with space", map[string]any{"name": "a b"}},
		{"name with path", map[string]any{"name": "../bin/sh"}},
		{"name with shell meta", map[string]any{"name": "a;b"}},
		{"name starts with dash", map[string]any{"name": "-rf"}},
		{"too many args", map[string]any{"name": "ok", "args": tooMany}},
		{"too long arg", map[string]any{"name": "ok", "args": []string{longArg}}},
		{"args not strings", map[string]any{"name": "ok", "args": []any{1}}},
		{"shell-string field", map[string]any{"name": "ok", "command": "rm -rf /"}},
		{"shell field", map[string]any{"name": "ok", "shell": "sh -c 'x'"}},
		{"cmd field", map[string]any{"name": "ok", "cmd": "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := postScriptAutopilot(t, "script-bad", "script", tc.cfg)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestCreateAutopilot_ScriptConfigRejectedForOtherModes(t *testing.T) {
	for _, mode := range []string{"create_issue", "run_only"} {
		w := postScriptAutopilot(t, "script-other-"+mode, mode, map[string]any{"name": "ok"})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s with script_config: want 400, got %d: %s", mode, w.Code, w.Body.String())
		}
	}
}

func TestCreateAutopilot_ExistingModesUnchanged(t *testing.T) {
	for _, mode := range []string{"create_issue", "run_only"} {
		w := postScriptAutopilot(t, "plain-"+mode, mode, nil)
		if w.Code != http.StatusCreated {
			t.Fatalf("%s: want 201, got %d: %s", mode, w.Code, w.Body.String())
		}
		var ap AutopilotResponse
		if err := json.Unmarshal(w.Body.Bytes(), &ap); err != nil {
			t.Fatal(err)
		}
		if ap.ExecutionMode != mode || (len(ap.ScriptConfig) != 0 && string(ap.ScriptConfig) != "null") {
			t.Fatalf("%s: mode=%q script_config=%s", mode, ap.ExecutionMode, ap.ScriptConfig)
		}
	}
	w := postScriptAutopilot(t, "plain-bogus", "bogus", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bogus mode: want 400, got %d", w.Code)
	}
}

func updateAutopilotBody(t *testing.T, id string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r := newRequest("PATCH", "/api/autopilots/"+id+"?workspace_id="+testWorkspaceID, body)
	testHandler.UpdateAutopilot(w, withURLParam(r, "id", id))
	return w
}

func TestUpdateAutopilot_ScriptMode(t *testing.T) {
	w := postScriptAutopilot(t, "script-upd", "script", map[string]any{"name": "first"})
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var ap AutopilotResponse
	json.Unmarshal(w.Body.Bytes(), &ap)

	if u := updateAutopilotBody(t, ap.ID, map[string]any{"script_config": map[string]any{"name": "second", "args": []string{"a"}}}); u.Code != http.StatusOK {
		t.Fatalf("valid update: %d %s", u.Code, u.Body.String())
	}
	for name, cfg := range map[string]any{
		"shell string": "sh -c x",
		"bad name":     map[string]any{"name": "a b"},
		"extra field":  map[string]any{"name": "ok", "command": "x"},
	} {
		if u := updateAutopilotBody(t, ap.ID, map[string]any{"script_config": cfg}); u.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d: %s", name, u.Code, u.Body.String())
		}
	}
	// Leaving script mode without clearing the config is refused.
	if u := updateAutopilotBody(t, ap.ID, map[string]any{"execution_mode": "run_only"}); u.Code != http.StatusBadRequest {
		t.Fatalf("leave script mode keeping config: want 400, got %d: %s", u.Code, u.Body.String())
	}
	if u := updateAutopilotBody(t, ap.ID, map[string]any{"execution_mode": "run_only", "script_config": nil}); u.Code != http.StatusOK {
		t.Fatalf("leave script mode clearing config: want 200, got %d: %s", u.Code, u.Body.String())
	}
}

func TestUpdateAutopilot_SwitchToScriptNeedsConfig(t *testing.T) {
	w := postScriptAutopilot(t, "script-switch", "run_only", nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d", w.Code)
	}
	var ap AutopilotResponse
	json.Unmarshal(w.Body.Bytes(), &ap)
	if u := updateAutopilotBody(t, ap.ID, map[string]any{"execution_mode": "script"}); u.Code != http.StatusBadRequest {
		t.Fatalf("script without config: want 400, got %d: %s", u.Code, u.Body.String())
	}
	if u := updateAutopilotBody(t, ap.ID, map[string]any{"execution_mode": "script", "script_config": map[string]any{"name": "ok"}}); u.Code != http.StatusOK {
		t.Fatalf("script with config: want 200, got %d: %s", u.Code, u.Body.String())
	}
	if u := updateAutopilotBody(t, ap.ID, map[string]any{"execution_mode": "nope"}); u.Code != http.StatusBadRequest {
		t.Fatalf("unknown mode: want 400, got %d: %s", u.Code, u.Body.String())
	}
}
