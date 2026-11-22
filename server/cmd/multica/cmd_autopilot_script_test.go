package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// CHE-5091 (CHE-4678 S9 P2): --mode script --script <name> [--arg <v>]...

const (
	scriptAgentID     = "11111111-1111-1111-1111-111111111111"
	scriptAutopilotID = "33333333-3333-3333-3333-333333333333"
)

func scriptCreateCmd() *cobra.Command {
	cmd := newAutopilotCreateTestCmd()
	cmd.Flags().String("script", "", "")
	cmd.Flags().StringArray("arg", nil, "")
	_ = cmd.Flags().Set("title", "Reconciler")
	_ = cmd.Flags().Set("agent", scriptAgentID)
	return cmd
}

func scriptUpdateCmd() *cobra.Command {
	cmd := newAutopilotUpdateTestCmd()
	cmd.Flags().String("script", "", "")
	cmd.Flags().StringArray("arg", nil, "")
	return cmd
}

// scriptServer records the last request body and echoes it back, as the real
// handler does with script_config.
func scriptServer(t *testing.T, body *map[string]any) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"id": scriptAutopilotID, "title": "Reconciler",
			"execution_mode": (*body)["execution_mode"],
			"script_config":  (*body)["script_config"],
		})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "test-token")
}

func TestAutopilotCommandsExposeScriptFlags(t *testing.T) {
	for name, cmd := range map[string]*cobra.Command{"create": autopilotCreateCmd, "update": autopilotUpdateCmd} {
		if err := cmd.ParseFlags([]string{"--script", "sweep", "--arg", "a", "--arg", "b"}); err != nil {
			t.Fatalf("autopilot %s: %v", name, err)
		}
		if got, _ := cmd.Flags().GetStringArray("arg"); !reflect.DeepEqual(got, []string{"a", "b"}) {
			t.Fatalf("autopilot %s args = %#v", name, got)
		}
	}
}

func TestRunAutopilotCreateSendsScriptConfig(t *testing.T) {
	var body map[string]any
	scriptServer(t, &body)

	cmd := scriptCreateCmd()
	_ = cmd.Flags().Set("mode", "script")
	_ = cmd.Flags().Set("script", "sweep-reconcile")
	_ = cmd.Flags().Set("arg", "--dry-run")
	_ = cmd.Flags().Set("arg", "x y")

	out, err := captureStdout(t, func() error { return runAutopilotCreate(cmd, nil) })
	if err != nil {
		t.Fatalf("runAutopilotCreate: %v", err)
	}
	if body["execution_mode"] != "script" {
		t.Fatalf("execution_mode = %#v", body["execution_mode"])
	}
	want := map[string]any{"name": "sweep-reconcile", "args": []any{"--dry-run", "x y"}}
	if !reflect.DeepEqual(body["script_config"], want) {
		t.Fatalf("script_config = %#v, want %#v", body["script_config"], want)
	}
	// --output json shows the script name and args.
	if !strings.Contains(out, "sweep-reconcile") || !strings.Contains(out, "x y") {
		t.Fatalf("json output lacks script name/args: %s", out)
	}
}

func TestRunAutopilotCreateScriptWithoutArgsSendsEmptyList(t *testing.T) {
	var body map[string]any
	scriptServer(t, &body)

	cmd := scriptCreateCmd()
	_ = cmd.Flags().Set("mode", "script")
	_ = cmd.Flags().Set("script", "sweep-reconcile")
	if err := runAutopilotCreate(cmd, nil); err != nil {
		t.Fatalf("runAutopilotCreate: %v", err)
	}
	want := map[string]any{"name": "sweep-reconcile", "args": []any{}}
	if !reflect.DeepEqual(body["script_config"], want) {
		t.Fatalf("script_config = %#v, want %#v", body["script_config"], want)
	}
}

func TestRunAutopilotCreateRejectsBadScriptFlagPairs(t *testing.T) {
	cases := []struct {
		name string
		set  map[string]string
		want string
	}{
		{"script without mode", map[string]string{"script": "sweep"}, "--mode script"},
		{"empty script without mode", map[string]string{"script": ""}, "--mode script"},
		{"script with other mode", map[string]string{"mode": "run_only", "script": "sweep"}, "--mode script"},
		{"mode script without script", map[string]string{"mode": "script"}, "--script"},
		{"arg without script mode", map[string]string{"mode": "run_only", "arg": "x"}, "--script"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			scriptServer(t, &body)
			cmd := scriptCreateCmd()
			for k, v := range tc.set {
				_ = cmd.Flags().Set(k, v)
			}
			err := runAutopilotCreate(cmd, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
			if body != nil {
				t.Fatalf("request sent despite rejected flags: %#v", body)
			}
		})
	}
}

func TestRunAutopilotUpdateSendsScriptConfig(t *testing.T) {
	var body map[string]any
	scriptServer(t, &body)

	cmd := scriptUpdateCmd()
	_ = cmd.Flags().Set("mode", "script")
	_ = cmd.Flags().Set("script", "sweep-reconcile")
	_ = cmd.Flags().Set("arg", "--once")
	if err := runAutopilotUpdate(cmd, []string{scriptAutopilotID}); err != nil {
		t.Fatalf("runAutopilotUpdate: %v", err)
	}
	if body["execution_mode"] != "script" {
		t.Fatalf("execution_mode = %#v", body["execution_mode"])
	}
	want := map[string]any{"name": "sweep-reconcile", "args": []any{"--once"}}
	if !reflect.DeepEqual(body["script_config"], want) {
		t.Fatalf("script_config = %#v, want %#v", body["script_config"], want)
	}
}

func TestRunAutopilotUpdateLeavingScriptModeClearsConfig(t *testing.T) {
	var body map[string]any
	scriptServer(t, &body)

	cmd := scriptUpdateCmd()
	_ = cmd.Flags().Set("mode", "run_only")
	if err := runAutopilotUpdate(cmd, []string{scriptAutopilotID}); err != nil {
		t.Fatalf("runAutopilotUpdate: %v", err)
	}
	cfg, ok := body["script_config"]
	if !ok || cfg != nil {
		t.Fatalf("script_config = %#v (present=%v), want explicit null", cfg, ok)
	}
}

func TestRunAutopilotUpdateRejectsBadScriptFlagPairs(t *testing.T) {
	cases := []struct {
		name string
		set  map[string]string
		want string
	}{
		{"script without mode", map[string]string{"script": "sweep"}, "--mode script"},
		{"empty script without mode", map[string]string{"script": ""}, "--mode script"},
		{"script with other mode", map[string]string{"mode": "create_issue", "script": "sweep"}, "--mode script"},
		{"mode script without script", map[string]string{"mode": "script"}, "--script"},
		{"arg without script", map[string]string{"arg": "x"}, "--script"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			scriptServer(t, &body)
			cmd := scriptUpdateCmd()
			for k, v := range tc.set {
				_ = cmd.Flags().Set(k, v)
			}
			err := runAutopilotUpdate(cmd, []string{scriptAutopilotID})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
			if body != nil {
				t.Fatalf("request sent despite rejected flags: %#v", body)
			}
		})
	}
}

func TestRunAutopilotCreateNeverSendsDescriptionAsScript(t *testing.T) {
	var body map[string]any
	scriptServer(t, &body)

	cmd := scriptCreateCmd()
	_ = cmd.Flags().Set("mode", "script")
	_ = cmd.Flags().Set("script", "sweep-reconcile")
	_ = cmd.Flags().Set("description", "rm -rf /")
	if err := runAutopilotCreate(cmd, nil); err != nil {
		t.Fatalf("runAutopilotCreate: %v", err)
	}
	cfg, _ := json.Marshal(body["script_config"])
	if strings.Contains(string(cfg), "rm -rf") {
		t.Fatalf("description leaked into script_config: %s", cfg)
	}
}
