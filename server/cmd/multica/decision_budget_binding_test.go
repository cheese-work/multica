package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/multica-ai/multica/server/internal/decisionaction"
)

// CHE-5228: run-budget changes use the CHE-2448 decision path. The CLI prints
// the digests (--print-hash) and sends a decision-bound body (--decision-id).

const (
	testBudgetProject  = "33333333-3333-3333-3333-333333333333"
	testBudgetWideFlow = "44444444-4444-4444-4444-444444444444"
)

func budgetInt(n int) *int { return &n }

// workflow update --max-runs-per-issue / --include-child-prs

func TestWorkflowBudgetPrintHashWritesNothing(t *testing.T) {
	var patched map[string]any
	var patches int32
	srv := workflowDecisionServer(t, &patched, &patches)
	defer srv.Close()
	clearAmbientDaemonTaskEnv(t)
	setCLITestServerEnv(t, srv.URL)

	cmd := newWorkflowDecisionCmd()
	_ = cmd.Flags().Set("max-runs-per-issue", "12")
	_ = cmd.Flags().Set("print-hash", "true")
	out, err := captureStdout(t, func() error { return runWorkflowUpdate(cmd, []string{testWorkflowID}) })
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&patches) != 0 {
		t.Errorf("PATCH calls = %d, want 0", patches)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout %q is not JSON: %v", out, err)
	}
	if want := decisionaction.WorkflowBudgetDigest(testWorkflowID, nil, true); got["before_sha256"] != want {
		t.Errorf("before_sha256 = %v, want %s", got["before_sha256"], want)
	}
	if want := decisionaction.WorkflowBudgetDigest(testWorkflowID, budgetInt(12), true); got["after_sha256"] != want {
		t.Errorf("after_sha256 = %v, want %s", got["after_sha256"], want)
	}
}

func TestWorkflowBudgetDecisionIDSendsBudgetOnlyBoundBody(t *testing.T) {
	var patched map[string]any
	var patches int32
	srv := workflowDecisionServer(t, &patched, &patches)
	defer srv.Close()
	clearAmbientDaemonTaskEnv(t)
	setCLITestServerEnv(t, srv.URL)

	cmd := newWorkflowDecisionCmd()
	_ = cmd.Flags().Set("max-runs-per-issue", "12")
	_ = cmd.Flags().Set("decision-id", testDecisionID)
	if _, err := captureStdout(t, func() error { return runWorkflowUpdate(cmd, []string{testWorkflowID}) }); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"max_runs_per_issue": float64(12),
		"decision_id":        testDecisionID,
		"before_sha256":      decisionaction.WorkflowBudgetDigest(testWorkflowID, nil, true),
		"after_sha256":       decisionaction.WorkflowBudgetDigest(testWorkflowID, budgetInt(12), true),
	}
	if len(patched) != len(want) {
		t.Fatalf("body = %v, want exactly %v", patched, want)
	}
	for k, v := range want {
		if patched[k] != v {
			t.Errorf("body[%q] = %v, want %v", k, patched[k], v)
		}
	}
}

func TestWorkflowBudgetDecisionIDCarriesChildPRSwitch(t *testing.T) {
	var patched map[string]any
	var patches int32
	srv := workflowDecisionServer(t, &patched, &patches)
	defer srv.Close()
	clearAmbientDaemonTaskEnv(t)
	setCLITestServerEnv(t, srv.URL)

	cmd := newWorkflowDecisionCmd()
	_ = cmd.Flags().Set("include-child-prs", "false")
	_ = cmd.Flags().Set("decision-id", testDecisionID)
	if _, err := captureStdout(t, func() error { return runWorkflowUpdate(cmd, []string{testWorkflowID}) }); err != nil {
		t.Fatal(err)
	}
	if patched["include_child_prs"] != false || patched["after_sha256"] != decisionaction.WorkflowBudgetDigest(testWorkflowID, nil, false) {
		t.Errorf("body = %v", patched)
	}
}

func TestWorkflowBudgetDecisionFlagErrorsSendNothing(t *testing.T) {
	steps := `[{"status_key":"done","handler":{"type":""},"instructions":""}]`
	cases := []struct {
		name string
		set  map[string]string
		want string
	}{
		{"budget with steps", map[string]string{"decision-id": testDecisionID, "steps-file": steps, "max-runs-per-issue": "5"}, "either --steps-file or the run budget flags"},
		{"budget with a rename", map[string]string{"decision-id": testDecisionID, "name": "x", "max-runs-per-issue": "5"}, "needs the owner/admin path"},
		{"cap out of range", map[string]string{"decision-id": testDecisionID, "max-runs-per-issue": "5000"}, "between 0 and"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var patched map[string]any
			var patches int32
			srv := workflowDecisionServer(t, &patched, &patches)
			defer srv.Close()
			clearAmbientDaemonTaskEnv(t)
			setCLITestServerEnv(t, srv.URL)

			cmd := newWorkflowDecisionCmd()
			cmd.Flags().String("name", "", "")
			for k, v := range tc.set {
				if k == "steps-file" {
					v = writeTemp(t, v)
				}
				if err := cmd.Flags().Set(k, v); err != nil {
					t.Fatal(err)
				}
			}
			err := runWorkflowUpdate(cmd, []string{testWorkflowID})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if atomic.LoadInt32(&patches) != 0 {
				t.Errorf("PATCH calls = %d, want 0", patches)
			}
		})
	}
}

// issue update --workflow / issue create --workflow

type issueWorkflowServer struct {
	*httptest.Server
	puts, posts []map[string]any
}

func newIssueWorkflowServer(t *testing.T) *issueWorkflowServer {
	t.Helper()
	clearAmbientDaemonTaskEnv(t)
	s := &issueWorkflowServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch {
		case r.Method == http.MethodGet && (r.URL.Path == "/api/issues/MUL-1" || r.URL.Path == "/api/issues/issue-1"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "issue-1", "identifier": "MUL-1", "status": "todo", "project_id": testBudgetProject})
		case r.Method == http.MethodGet && (r.URL.Path == "/api/issues/MUL-9" || r.URL.Path == "/api/issues/issue-9"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "issue-9", "identifier": "MUL-9", "status": "todo", "project_id": testBudgetProject})
		case r.Method == http.MethodPut && r.URL.Path == "/api/issues/issue-1/workflow":
			s.puts = append(s.puts, body)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "issue-1", "identifier": "MUL-1", "status": "todo"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/issues":
			s.posts = append(s.posts, body)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "issue-2", "identifier": "MUL-2", "title": "t", "status": "todo", "priority": "none"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Setenv("MULTICA_SERVER_URL", s.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "test-token")
	return s
}

func TestIssueUpdateWorkflowPrintHashWritesNothing(t *testing.T) {
	clearAmbientDaemonTaskEnv(t)
	srv := newIssueWorkflowServer(t)
	defer srv.Close()

	cmd := newIssueUpdateTestCmd()
	_ = cmd.Flags().Set("workflow", testBudgetWideFlow)
	_ = cmd.Flags().Set("print-hash", "true")
	out, err := captureStdout(t, func() error { return runIssueUpdate(cmd, []string{"MUL-1"}) })
	if err != nil {
		t.Fatal(err)
	}
	if len(srv.puts) != 0 {
		t.Errorf("PUT calls = %d, want 0", len(srv.puts))
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout %q is not JSON: %v", out, err)
	}
	if want := decisionaction.IssueWorkflowDigest("issue-1", testBudgetProject, ""); got["before_sha256"] != want {
		t.Errorf("before_sha256 = %v, want %s", got["before_sha256"], want)
	}
	if want := decisionaction.IssueWorkflowDigest("issue-1", testBudgetProject, testBudgetWideFlow); got["after_sha256"] != want {
		t.Errorf("after_sha256 = %v, want %s", got["after_sha256"], want)
	}
}

func TestIssueUpdateWorkflowDecisionIDSendsBoundBody(t *testing.T) {
	clearAmbientDaemonTaskEnv(t)
	srv := newIssueWorkflowServer(t)
	defer srv.Close()

	cmd := newIssueUpdateTestCmd()
	_ = cmd.Flags().Set("workflow", testBudgetWideFlow)
	_ = cmd.Flags().Set("decision-id", testDecisionID)
	if _, err := captureStdout(t, func() error { return runIssueUpdate(cmd, []string{"MUL-1"}) }); err != nil {
		t.Fatal(err)
	}
	if len(srv.puts) != 1 {
		t.Fatalf("PUT calls = %d, want 1", len(srv.puts))
	}
	want := map[string]any{
		"workflow_id":   testBudgetWideFlow,
		"decision_id":   testDecisionID,
		"before_sha256": decisionaction.IssueWorkflowDigest("issue-1", testBudgetProject, ""),
		"after_sha256":  decisionaction.IssueWorkflowDigest("issue-1", testBudgetProject, testBudgetWideFlow),
	}
	for k, v := range want {
		if srv.puts[0][k] != v {
			t.Errorf("body[%q] = %v, want %v", k, srv.puts[0][k], v)
		}
	}
}

func TestIssueUpdateDecisionFlagsNeedAWorkflow(t *testing.T) {
	clearAmbientDaemonTaskEnv(t)
	srv := newIssueWorkflowServer(t)
	defer srv.Close()

	for name, set := range map[string]map[string]string{
		"print-hash alone":  {"print-hash": "true"},
		"decision-id alone": {"decision-id": testDecisionID, "title": "x"},
		"bad decision id":   {"decision-id": "CHE-5228", "workflow": testBudgetWideFlow},
	} {
		cmd := newIssueUpdateTestCmd()
		for k, v := range set {
			_ = cmd.Flags().Set(k, v)
		}
		err := runIssueUpdate(cmd, []string{"MUL-1"})
		if err == nil || (!strings.Contains(err.Error(), "--workflow") && !strings.Contains(err.Error(), "decision request UUID")) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if len(srv.puts) != 0 {
		t.Errorf("PUT calls = %d, want 0", len(srv.puts))
	}
}

func TestIssueCreateWorkflowPrintHashWritesNothing(t *testing.T) {
	clearAmbientDaemonTaskEnv(t)
	srv := newIssueWorkflowServer(t)
	defer srv.Close()

	cmd := newIssueCreateTestCmd()
	_ = cmd.Flags().Set("title", "t")
	_ = cmd.Flags().Set("project", testBudgetProject)
	_ = cmd.Flags().Set("workflow", testBudgetWideFlow)
	_ = cmd.Flags().Set("print-hash", "true")
	out, err := captureStdout(t, func() error { return runIssueCreate(cmd, nil) })
	if err != nil {
		t.Fatal(err)
	}
	if len(srv.posts) != 0 {
		t.Errorf("POST calls = %d, want 0", len(srv.posts))
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout %q is not JSON: %v", out, err)
	}
	if want := decisionaction.IssueWorkflowDigest("", testBudgetProject, ""); got["before_sha256"] != want {
		t.Errorf("before_sha256 = %v, want %s", got["before_sha256"], want)
	}
	if want := decisionaction.IssueWorkflowDigest("", testBudgetProject, testBudgetWideFlow); got["after_sha256"] != want {
		t.Errorf("after_sha256 = %v, want %s", got["after_sha256"], want)
	}
}

// Without --project the issue follows its parent's project, as on the server.
func TestIssueCreateWorkflowDecisionIDUsesTheParentsProject(t *testing.T) {
	clearAmbientDaemonTaskEnv(t)
	srv := newIssueWorkflowServer(t)
	defer srv.Close()

	cmd := newIssueCreateTestCmd()
	_ = cmd.Flags().Set("title", "t")
	_ = cmd.Flags().Set("parent", "MUL-9")
	_ = cmd.Flags().Set("workflow", testBudgetWideFlow)
	_ = cmd.Flags().Set("decision-id", testDecisionID)
	if _, err := captureStdout(t, func() error { return runIssueCreate(cmd, nil) }); err != nil {
		t.Fatal(err)
	}
	if len(srv.posts) != 1 {
		t.Fatalf("POST calls = %d, want 1", len(srv.posts))
	}
	body := srv.posts[0]
	if body["decision_id"] != testDecisionID || body["workflow_id"] != testBudgetWideFlow ||
		body["before_sha256"] != decisionaction.IssueWorkflowDigest("", testBudgetProject, "") ||
		body["after_sha256"] != decisionaction.IssueWorkflowDigest("", testBudgetProject, testBudgetWideFlow) {
		t.Errorf("body = %v", body)
	}
}
