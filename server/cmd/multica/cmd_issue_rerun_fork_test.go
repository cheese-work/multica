package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestIssueRerunCommandsRegisterReason pins the fork's audited-force flag
// (CHE-525) on the registered commands, not on a test-built copy: upstream's
// deprecated `issue rerun` keeps --reason, and its replacement `rerun-task`
// carries it too so the deprecation notice does not steer callers off the
// audited path.
func TestIssueRerunCommandsRegisterReason(t *testing.T) {
	if issueRerunCmd.Flags().Lookup("reason") == nil {
		t.Error("`issue rerun` lost its --reason flag")
	}
	if issueRerunTaskCmd.Flags().Lookup("reason") == nil {
		t.Error("`issue rerun-task` has no --reason flag")
	}
}

// TestRunIssueRerunTaskSendsReason proves --reason reaches the run-targeted
// rerun endpoint beside `fresh`, so the handler can write it to the audit log.
func TestRunIssueRerunTaskSendsReason(t *testing.T) {
	t.Chdir(t.TempDir())
	const runID = "aaaaaaaa-1111-4222-8333-444444444444"
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/tasks/"+runID+"/rerun" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "new-run", "agent_id": "agent-1"})
	}))
	defer srv.Close()
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "mat_test-token")

	cmd := issueRerunTaskCmd
	for flag, value := range map[string]string{"reason": "authorized force: misclassified", "fresh": "true"} {
		if err := cmd.Flags().Set(flag, value); err != nil {
			t.Fatalf("set --%s: %v", flag, err)
		}
	}
	t.Cleanup(func() {
		_ = cmd.Flags().Set("reason", "")
		_ = cmd.Flags().Set("fresh", "false")
	})
	if _, err := captureStdout(t, func() error { return runIssueRerunTask(cmd, []string{runID}) }); err != nil {
		t.Fatal(err)
	}
	if body["reason"] != "authorized force: misclassified" || body["fresh"] != true {
		t.Fatalf("body = %v, want reason and fresh=true", body)
	}
}
