package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func newAgentEnvPatchTestCmd() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().String("custom-env", "", "")
	cmd.Flags().Bool("custom-env-stdin", false, "")
	cmd.Flags().String("custom-env-file", "", "")
	cmd.Flags().StringSlice("unset", nil, "")
	cmd.Flags().String("if-revision", "", "")
	cmd.Flags().String("output", "json", "")
	cmd.Flags().String("profile", "", "")
	return cmd
}

func TestRunAgentEnvPatchSendsKeyScopedConditionalBody(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("MULTICA_AGENT_ID", "")
	t.Setenv("MULTICA_TASK_ID", "")

	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/api/agents/agent-1/env" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"agent_id":"agent-1","custom_env":{"A":"****"},"revision":"r2"}`))
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)

	cmd := newAgentEnvPatchTestCmd()
	_ = cmd.Flags().Set("custom-env", `{"A":"1"}`)
	_ = cmd.Flags().Set("unset", "B")
	_ = cmd.Flags().Set("if-revision", "r1")
	out, err := captureStdout(t, func() error { return runAgentEnvPatch(cmd, []string{"agent-1"}) })
	if err != nil {
		t.Fatal(err)
	}
	if got["if_revision"] != "r1" || got["set"].(map[string]any)["A"] != "1" || got["unset"].([]any)[0] != "B" {
		t.Fatalf("body = %v", got)
	}
	if !strings.Contains(out, `"r2"`) {
		t.Fatalf("output = %s", out)
	}
}

func TestRunAgentEnvPatchRequiresAChange(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("MULTICA_AGENT_ID", "")
	t.Setenv("MULTICA_TASK_ID", "")
	setCLITestServerEnv(t, "http://127.0.0.1:1")
	if err := runAgentEnvPatch(newAgentEnvPatchTestCmd(), []string{"agent-1"}); err == nil {
		t.Fatal("expected an error when nothing to set or unset")
	}
}

func TestRunAgentEnvPatchSurfacesStaleRevision(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("MULTICA_AGENT_ID", "")
	t.Setenv("MULTICA_TASK_ID", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"env changed since if_revision"}`, http.StatusPreconditionFailed)
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)

	cmd := newAgentEnvPatchTestCmd()
	_ = cmd.Flags().Set("unset", "B")
	_ = cmd.Flags().Set("if-revision", "stale")
	if err := runAgentEnvPatch(cmd, []string{"agent-1"}); err == nil || !strings.Contains(err.Error(), "412") {
		t.Fatalf("err = %v, want 412", err)
	}
}
