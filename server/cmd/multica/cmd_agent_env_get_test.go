package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const plantedCLIEnvValue = "example-planted-7f3c9e"

// A8a: `agent env get` shows key and source only. The server body here still
// carries a value, so the table proves the CLI never prints one.
func TestRunAgentEnvGetTableShowsKeySourceOnly(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("MULTICA_AGENT_ID", "")
	t.Setenv("MULTICA_TASK_ID", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"agent_id":"agent-1","custom_env":{"A":"` + plantedCLIEnvValue + `"},"keys":[{"key":"A","source":"legacy"}],"revision":"v3"}`))
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)

	cmd := newAgentEnvPatchTestCmd()
	_ = cmd.Flags().Set("output", "table")
	out, err := captureStdout(t, func() error { return runAgentEnvGet(cmd, []string{"agent-1"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "KEY") || !strings.Contains(out, "SOURCE") || !strings.Contains(out, "legacy") || strings.Contains(out, "VALUE") {
		t.Fatalf("table = %q, want KEY/SOURCE columns", out)
	}
	if strings.Contains(out, plantedCLIEnvValue) {
		t.Fatalf("table leaks the value: %q", out)
	}
}
