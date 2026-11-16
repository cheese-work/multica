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

// runWakeupDefinition runs the command against a stub server and returns the
// request it saw plus stdout.
func runWakeupDefinition(t *testing.T, status int, reply any, args ...string) (method, path, rawQuery string, body map[string]any, out string, err error) {
	t.Helper()
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, rawQuery = r.Method, r.URL.Path, r.URL.RawQuery
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(status)
		if reply != nil {
			_ = json.NewEncoder(w).Encode(reply)
		}
	}))
	defer srv.Close()
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "test-token")
	var buf bytes.Buffer
	cmd := newWakeupDefinitionCommand()
	cmd.SetOut(&buf)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs(args)
	err = cmd.Execute()
	return method, path, rawQuery, body, buf.String(), err
}

func TestWakeupDefinitionListCLI(t *testing.T) {
	const project = "a57c0511-1ebc-471d-a314-438ca16cc75d"
	reply := map[string]any{"definitions": []map[string]any{
		{"rule_key": "pr_merged", "scope": "workspace", "revision": "3", "config": map[string]any{"v": 1, "enabled": true}},
	}}
	for _, tc := range []struct {
		name, path string
		args       []string
	}{
		{"workspace", "/api/wakeup-definitions/", nil},
		{"project", "/api/projects/" + project + "/wakeup-definitions/", []string{"--project", project}},
	} {
		for _, format := range []string{"json", "table"} {
			t.Run(tc.name+"/"+format, func(t *testing.T) {
				m, p, _, _, out, err := runWakeupDefinition(t, 200, reply, append([]string{"list", "--output", format}, tc.args...)...)
				if err != nil {
					t.Fatal(err)
				}
				if m != "GET" || strings.TrimSuffix(p, "/") != strings.TrimSuffix(tc.path, "/") {
					t.Fatalf("unexpected request %s %s", m, p)
				}
				want := `"rule_key"`
				if format == "table" {
					want = "RULE"
				}
				if !strings.Contains(out, want) || !strings.Contains(out, "pr_merged") {
					t.Fatalf("%s output missing %q: %s", format, want, out)
				}
			})
		}
	}
}

func TestWakeupDefinitionUpdateCLI(t *testing.T) {
	const project = "a57c0511-1ebc-471d-a314-438ca16cc75d"
	reply := map[string]any{"rule_key": "pr_merged", "revision": "4", "config": map[string]any{"v": 1, "enabled": true}}
	m, p, _, body, out, err := runWakeupDefinition(t, 200, reply, "update", "pr_merged", "--revision", "3", "--config", `{"v":1,"enabled":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if m != "PUT" || p != "/api/wakeup-definitions/pr_merged" {
		t.Fatalf("unexpected request %s %s", m, p)
	}
	cfg, _ := body["config"].(map[string]any)
	if cfg["enabled"] != true || body["revision"] != "3" {
		t.Fatalf("body not passed through: %+v", body)
	}
	if !strings.Contains(out, `"revision": "4"`) {
		t.Fatalf("stdout does not show the stored definition: %s", out)
	}
	_, p, _, _, _, err = runWakeupDefinition(t, 200, reply, "update", "pr_merged", "--revision", "3", "--config", `{"v":1}`, "--project", project)
	if err != nil || p != "/api/projects/"+project+"/wakeup-definitions/pr_merged" {
		t.Fatalf("project path %q err %v", p, err)
	}
	if _, _, _, _, _, err = runWakeupDefinition(t, 200, reply, "update", "pr_merged", "--config", `{"v":1}`); err == nil || !strings.Contains(err.Error(), "--revision") {
		t.Fatalf("want a --revision error, got %v", err)
	}
	if _, _, _, _, _, err = runWakeupDefinition(t, 200, reply, "update", "pr_merged", "--revision", "3"); err == nil || !strings.Contains(err.Error(), "--config") {
		t.Fatalf("want a --config error, got %v", err)
	}
}

func TestWakeupDefinitionDeleteCLI(t *testing.T) {
	const project = "a57c0511-1ebc-471d-a314-438ca16cc75d"
	m, p, q, _, _, err := runWakeupDefinition(t, 204, nil, "delete", "pr_merged", "--revision", "3")
	if err != nil {
		t.Fatal(err)
	}
	if m != "DELETE" || p != "/api/wakeup-definitions/pr_merged" || q != "revision=3" {
		t.Fatalf("unexpected request %s %s?%s", m, p, q)
	}
	_, p, _, _, _, err = runWakeupDefinition(t, 204, nil, "delete", "pr_merged", "--revision", "3", "--project", project)
	if err != nil || p != "/api/projects/"+project+"/wakeup-definitions/pr_merged" {
		t.Fatalf("project path %q err %v", p, err)
	}
	if _, _, _, _, _, err = runWakeupDefinition(t, 204, nil, "delete", "pr_merged"); err == nil || !strings.Contains(err.Error(), "--revision") {
		t.Fatalf("want a --revision error, got %v", err)
	}
}

func TestWakeupDefinitionPreviewCLI(t *testing.T) {
	const project = "a57c0511-1ebc-471d-a314-438ca16cc75d"
	reply := map[string]any{"rule_key": "pr_merged", "already_satisfied": map[string]any{"satisfied": 2, "examined": 10}}
	m, p, _, body, out, err := runWakeupDefinition(t, 200, reply, "preview", "--rule", "pr_merged", "--config", `{"v":1,"enabled":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if m != "POST" || p != "/api/wakeup-definitions/preview" {
		t.Fatalf("unexpected request %s %s", m, p)
	}
	if body["rule_key"] != "pr_merged" || body["config"] == nil {
		t.Fatalf("body not passed through: %+v", body)
	}
	if !strings.Contains(out, `"satisfied": 2`) {
		t.Fatalf("stdout does not show the match count: %s", out)
	}
	_, p, _, body, _, err = runWakeupDefinition(t, 200, reply, "preview", "--config", `{"v":1}`, "--project", project)
	if err != nil || p != "/api/projects/"+project+"/wakeup-definitions/preview" {
		t.Fatalf("project path %q err %v", p, err)
	}
	if _, set := body["rule_key"]; set {
		t.Fatalf("new-rule preview must omit rule_key: %+v", body)
	}
}

// --output table names the issues a condition rule would already match, says
// when the preview saw only the first part of the scope, and says plainly that
// an event rule has nothing to match.
func TestWakeupDefinitionPreviewTableNamesTheMatchingIssues(t *testing.T) {
	cfg := []string{"preview", "--config", `{"v":1}`, "--output", "table"}
	reply := map[string]any{"already_satisfied": map[string]any{
		"satisfied": 2, "examined": 5, "truncated": false,
		"issues": []map[string]any{
			{"id": "i1", "identifier": "CHE-12", "title": "Fix login"},
			{"id": "i2", "identifier": "CHE-15", "title": "Add export"},
		},
	}}
	_, _, _, _, out, err := runWakeupDefinition(t, 200, reply, cfg...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CHE-12", "Fix login", "CHE-15", "Add export", "2 of 5"} {
		if !strings.Contains(out, want) {
			t.Fatalf("table lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "first") {
		t.Fatalf("a complete preview must not claim truncation:\n%s", out)
	}

	reply["already_satisfied"] = map[string]any{"satisfied": 1, "examined": 200, "truncated": true, "issues": []map[string]any{{"id": "i1", "identifier": "CHE-12", "title": "Fix login"}}}
	_, _, _, _, out, err = runWakeupDefinition(t, 200, reply, cfg...)
	if err != nil || !strings.Contains(out, "first 200") || !strings.Contains(out, "incomplete") || !strings.Contains(out, "CHE-12") {
		t.Fatalf("a truncated preview must say it is incomplete (err %v):\n%s", err, out)
	}

	_, _, _, _, out, err = runWakeupDefinition(t, 200, map[string]any{"rule_key": "x"}, cfg...)
	if err != nil || !strings.Contains(out, "event") || !strings.Contains(out, "no existing issue") {
		t.Fatalf("an event rule must be reported as matching no existing issue (err %v):\n%s", err, out)
	}
}

// The commands add no bypass: a server 403 (an agent may only create a rule)
// reaches the caller as an error.
func TestWakeupDefinitionWriteCommandsSurfaceServerGate(t *testing.T) {
	forbidden := map[string]any{"error": "agents cannot edit wakeup definitions"}
	for _, args := range [][]string{
		{"update", "pr_merged", "--revision", "1", "--config", `{"v":1}`},
		{"delete", "pr_merged", "--revision", "1"},
		{"preview", "--config", `{"v":1}`},
	} {
		if _, _, _, _, _, err := runWakeupDefinition(t, 403, forbidden, args...); err == nil || !strings.Contains(err.Error(), "403") {
			t.Errorf("%s: want a 403 error, got %v", args[0], err)
		}
	}
}
