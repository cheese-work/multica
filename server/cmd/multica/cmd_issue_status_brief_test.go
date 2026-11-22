package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A chained workflow move starts no run, so the server returns the entered
// step's text as step_brief; the CLI prints it where the agent reads it.
func TestIssueStatusPrintsStepBrief(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/issues/MUL-2":
			json.NewEncoder(w).Encode(map[string]any{"id": "issue-2", "identifier": "MUL-2", "status": "todo"})
		case r.Method == http.MethodPut && r.URL.Path == "/api/issues/issue-2":
			json.NewEncoder(w).Encode(map[string]any{
				"id": "issue-2", "identifier": "MUL-2", "status": "in_progress",
				"step_brief": "Implement it, then hand over.",
			})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	setCLITestServerEnv(t, srv.URL)
	t.Setenv("MULTICA_TASK_CONFIG_ROOT", t.TempDir())
	t.Setenv("MULTICA_TOKEN", "mat_test-token")

	cmd := newIssueStatusTestCmd()
	out, err := captureStdout(t, func() error {
		return runIssueStatus(cmd, []string{"MUL-2", "in_progress"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Implement it, then hand over.") {
		t.Fatalf("stdout = %q, want the step brief", out)
	}
}
