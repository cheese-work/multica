package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
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

// Check 3: stdout and stderr of get (table, JSON), set --output json and
// patch --output json hold no planted value. Write inputs carry the value; the
// fake server answers with the masked body the real server sends.
func TestRunAgentEnvCommandsHoldNoPlantedValue(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("MULTICA_AGENT_ID", "")
	t.Setenv("MULTICA_TASK_ID", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"agent_id":"agent-1","custom_env":{"A":"****"},"keys":[{"key":"A","source":"legacy"}],"revision":"v4"}`))
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)

	planted := `{"A":"` + plantedCLIEnvValue + `"}`
	cases := []struct {
		name string
		run  func(*cobra.Command) error
		set  map[string]string
	}{
		{"get table", func(c *cobra.Command) error { return runAgentEnvGet(c, []string{"agent-1"}) }, map[string]string{"output": "table"}},
		{"get json", func(c *cobra.Command) error { return runAgentEnvGet(c, []string{"agent-1"}) }, map[string]string{"output": "json"}},
		{"set json", func(c *cobra.Command) error { return runAgentEnvSet(c, []string{"agent-1"}) }, map[string]string{"output": "json", "custom-env": planted}},
		{"patch json", func(c *cobra.Command) error { return runAgentEnvPatch(c, []string{"agent-1"}) }, map[string]string{"output": "json", "custom-env": planted}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newAgentEnvPatchTestCmd()
			for k, v := range tc.set {
				if err := cmd.Flags().Set(k, v); err != nil {
					t.Fatal(err)
				}
			}
			errCap := captureStderr(t)
			out, err := captureStdout(t, func() error { return tc.run(cmd) })
			stderr := errCap.read()
			if err != nil {
				t.Fatal(err)
			}
			if out == "" {
				t.Fatal("stdout is empty; the assertion would pass vacuously")
			}
			if strings.Contains(out, plantedCLIEnvValue) {
				t.Errorf("stdout leaks the planted value: %q", out)
			}
			if strings.Contains(stderr, plantedCLIEnvValue) {
				t.Errorf("stderr leaks the planted value: %q", stderr)
			}
		})
	}
}
