package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWakeupDefinitionCreateCLIPrintsStoredDisabled(t *testing.T) {
	const project = "a57c0511-1ebc-471d-a314-438ca16cc75d"
	for _, tc := range []struct {
		name, path string
		args       []string
	}{
		{"workspace", "/api/wakeup-definitions", nil},
		{"project", "/api/projects/" + project + "/wakeup-definitions", []string{"--project", project}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			var got map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != tc.path {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				_ = json.NewDecoder(r.Body).Decode(&got)
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]any{"rule_key": "custom:1", "config": map[string]any{"v": 1, "enabled": false}})
			}))
			defer srv.Close()
			t.Setenv("MULTICA_SERVER_URL", srv.URL)
			t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
			t.Setenv("MULTICA_TOKEN", "test-token")
			var out, errOut bytes.Buffer
			cmd := newWakeupDefinitionCommand()
			cmd.SetOut(&out)
			cmd.SetErr(&errOut)
			cmd.SetArgs(append([]string{"create", "--config", `{"v":1,"enabled":true,"instruction":"x"}`}, tc.args...))
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			cfg, _ := got["config"].(map[string]any)
			if cfg["instruction"] != "x" {
				t.Fatalf("config not passed through: %+v", got)
			}
			if !strings.Contains(out.String(), `"enabled": false`) {
				t.Fatalf("stdout does not show the stored config: %s", out.String())
			}
			if !strings.Contains(errOut.String(), "stored disabled") {
				t.Fatalf("stderr does not say the rule is disabled: %s", errOut.String())
			}
		})
	}
}
