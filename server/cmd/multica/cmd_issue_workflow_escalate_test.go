package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// CHE-7682: `multica issue workflow escalate <id> --reason ...` posts the
// reason and nothing else; the server names the target.

func newEscalateTestServer(t *testing.T, status int, reply map[string]any, body *map[string]any) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/issues/MUL-1":
			json.NewEncoder(w).Encode(map[string]any{"id": "issue-1", "identifier": "MUL-1", "status": "in_progress"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/issues/issue-1/workflow/escalate":
			if err := json.NewDecoder(r.Body).Decode(body); err != nil {
				t.Errorf("decode body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(reply)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	setCLITestServerEnv(t, srv.URL)
	t.Setenv("MULTICA_TASK_CONFIG_ROOT", t.TempDir())
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
}

func TestIssueWorkflowEscalateTakesAReasonAndNoTarget(t *testing.T) {
	if issueWorkflowEscalateCmd.Flags().Lookup("reason") == nil {
		t.Fatal("issue workflow escalate has no --reason flag")
	}
	for _, name := range []string{"workflow", "workflow-id", "target"} {
		if issueWorkflowEscalateCmd.Flags().Lookup(name) != nil {
			t.Fatalf("issue workflow escalate has a --%s flag; the server names the target", name)
		}
	}
}

func TestRunIssueWorkflowEscalateSendsOnlyTheReason(t *testing.T) {
	var body map[string]any
	newEscalateTestServer(t, http.StatusOK, map[string]any{"state": "applied", "workflow_id": "wf-full"}, &body)
	if err := issueWorkflowEscalateCmd.Flags().Set("reason", "needs a schema change"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { issueWorkflowEscalateCmd.Flags().Set("reason", "") })

	if err := runIssueWorkflowEscalate(issueWorkflowEscalateCmd, []string{"MUL-1"}); err != nil {
		t.Fatalf("escalate: %v", err)
	}
	if len(body) != 1 || body["reason"] != "needs a schema change" {
		t.Fatalf("body = %v, want only the reason", body)
	}
}

func TestRunIssueWorkflowEscalateRequiresAReason(t *testing.T) {
	var body map[string]any
	newEscalateTestServer(t, http.StatusOK, map[string]any{}, &body)
	issueWorkflowEscalateCmd.Flags().Set("reason", "")
	err := runIssueWorkflowEscalate(issueWorkflowEscalateCmd, []string{"MUL-1"})
	if err == nil || !strings.Contains(err.Error(), "--reason") {
		t.Fatalf("err = %v, want a --reason error", err)
	}
	if body != nil {
		t.Fatalf("a request was sent: %v", body)
	}
}

func TestRunIssueWorkflowEscalateReportsARefusal(t *testing.T) {
	var body map[string]any
	newEscalateTestServer(t, http.StatusConflict, map[string]any{"error": "the issue has no record of entering Quick", "code": "no_full_workflow"}, &body)
	issueWorkflowEscalateCmd.Flags().Set("reason", "x")
	t.Cleanup(func() { issueWorkflowEscalateCmd.Flags().Set("reason", "") })

	err := runIssueWorkflowEscalate(issueWorkflowEscalateCmd, []string{"MUL-1"})
	if err == nil || !strings.Contains(err.Error(), "no record of entering Quick") {
		t.Fatalf("err = %v, want the server's refusal", err)
	}
}
