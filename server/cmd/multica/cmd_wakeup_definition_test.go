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

func TestWakeupDefinitionGetCLI(t *testing.T) {
	const id = "a57c0511-1ebc-471d-a314-438ca16cc75d"
	eff := map[string]any{
		"rule_key": "pr_merged", "scope": "x", "applicable": true, "enabled": true,
		"config":    map[string]any{"v": 1, "instruction": "check-the-pr"},
		"sources":   map[string]string{"instruction": "workspace"},
		"overrides": []string{"enabled"},
		"execution": map[string]any{"instance_id": "w-1", "enabled": false, "paused_reason": "loop"},
	}
	for _, tc := range []struct {
		name, path string
		args       []string
	}{
		{"workspace", "/api/wakeup-definitions/pr_merged/effective", nil},
		{"project", "/api/projects/" + id + "/wakeup-definitions/pr_merged/effective", []string{"--project", id}},
		{"issue", "/api/issues/" + id + "/wakeup-definitions/pr_merged/effective", []string{"--issue", id}},
	} {
		for _, format := range []string{"json", "table"} {
			t.Run(tc.name+"/"+format, func(t *testing.T) {
				t.Chdir(t.TempDir())
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != "GET" || r.URL.Path != tc.path {
						t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					}
					_ = json.NewEncoder(w).Encode(eff)
				}))
				defer srv.Close()
				t.Setenv("MULTICA_SERVER_URL", srv.URL)
				t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
				t.Setenv("MULTICA_TOKEN", "test-token")
				var out bytes.Buffer
				cmd := newWakeupDefinitionCommand()
				cmd.SetOut(&out)
				cmd.SetArgs(append([]string{"get", "pr_merged", "--output", format}, tc.args...))
				if err := cmd.Execute(); err != nil {
					t.Fatal(err)
				}
				wants := []string{`"sources"`, `"overrides"`, `"execution"`}
				if format == "table" {
					wants = []string{"pr_merged", "instruction=workspace", "enabled", "loop", "CONFIG", `"instruction":"check-the-pr"`}
				}
				for _, want := range wants {
					if !strings.Contains(out.String(), want) {
						t.Errorf("%s output missing %q: %s", format, want, out.String())
					}
				}
			})
		}
	}
}

func TestWakeupDefinitionGetCLIRejectsProjectAndIssue(t *testing.T) {
	cmd := newWakeupDefinitionCommand()
	cmd.SetArgs([]string{"get", "pr_merged", "--project", "p", "--issue", "i"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--project") {
		t.Fatalf("want a --project/--issue conflict error, got %v", err)
	}
}
