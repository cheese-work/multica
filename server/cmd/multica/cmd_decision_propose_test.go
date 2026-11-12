package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func newDecisionProposeTestCmd() *cobra.Command {
	c := &cobra.Command{Use: "propose"}
	c.Flags().String("issue", "", "")
	c.Flags().Int("version", 0, "")
	c.Flags().String("for", "", "")
	c.Flags().StringArray("param", nil, "")
	c.Flags().String("params-file", "", "")
	c.Flags().String("reason", "", "")
	c.Flags().String("output", "table", "")
	return c
}

// proposalServer records proposal POSTs and answers with status/body; every
// other path is recorded in other so a test can prove a route was not used.
func proposalServer(t *testing.T, status int, reply map[string]any, posts *[]map[string]any, other *[]string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/decision-actions/proposals":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			*posts = append(*posts, body)
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(reply)
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/ws-1":
			_ = json.NewEncoder(w).Encode(map[string]any{"slug": "acme"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/issues/"+testIssueUUID+"/run-budget":
			_ = json.NewEncoder(w).Encode(map[string]any{"budgeted": true, "window_id": "w-1", "raise_preview": map[string]any{"limit_after": 55}})
		default:
			*other = append(*other, r.Method+" "+r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
}

var proposalCreated = map[string]any{"id": "req-1", "action": map[string]any{"id": "act-1"}}

func TestDecisionProposeEncodesParams(t *testing.T) {
	var posts []map[string]any
	var other []string
	proposalServer(t, http.StatusCreated, proposalCreated, &posts, &other)
	cmd := newDecisionProposeTestCmd()
	_ = cmd.Flags().Set("issue", testIssueUUID)
	_ = cmd.Flags().Set("version", "1")
	_ = cmd.Flags().Set("reason", "cap is too low")
	for _, p := range []string{"window_id=w-1", "new_cap:=71", "note=a=b"} {
		_ = cmd.Flags().Set("param", p)
	}
	if _, err := captureStdout(t, func() error { return runDecisionPropose(cmd, []string{"issue.run_budget.raise"}) }); err != nil {
		t.Fatalf("propose: %v", err)
	}
	want := map[string]any{
		"action_type": "issue.run_budget.raise", "version": float64(1), "issue_id": testIssueUUID, "reason": "cap is too low",
		"params": map[string]any{"window_id": "w-1", "new_cap": float64(71), "note": "a=b"},
	}
	if len(posts) != 1 || !reflect.DeepEqual(posts[0], want) {
		t.Fatalf("posts = %v, want [%v]", posts, want)
	}
}

func TestDecisionProposeReadsParamsFile(t *testing.T) {
	var posts []map[string]any
	var other []string
	proposalServer(t, http.StatusCreated, proposalCreated, &posts, &other)
	file := filepath.Join(t.TempDir(), "params.json")
	if err := os.WriteFile(file, []byte(`{"agent_id":"a-1","name":"copy"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newDecisionProposeTestCmd()
	_ = cmd.Flags().Set("issue", testIssueUUID)
	_ = cmd.Flags().Set("version", "1")
	_ = cmd.Flags().Set("for", "user-1")
	_ = cmd.Flags().Set("params-file", file)
	if _, err := captureStdout(t, func() error { return runDecisionPropose(cmd, []string{"agent.copy"}) }); err != nil {
		t.Fatalf("propose: %v", err)
	}
	want := map[string]any{"agent_id": "a-1", "name": "copy"}
	if len(posts) != 1 || !reflect.DeepEqual(posts[0]["params"], want) || posts[0]["for"] != "user-1" {
		t.Fatalf("posts = %v, want params %v for user-1", posts, want)
	}
}

func TestDecisionProposeRefusesBadParams(t *testing.T) {
	var posts []map[string]any
	var other []string
	proposalServer(t, http.StatusCreated, proposalCreated, &posts, &other)
	for name, set := range map[string]func(*cobra.Command){
		"no equals":        func(c *cobra.Command) { _ = c.Flags().Set("param", "oops") },
		"bad json":         func(c *cobra.Command) { _ = c.Flags().Set("param", "n:={") },
		"both sources":     func(c *cobra.Command) { _ = c.Flags().Set("param", "a=b"); _ = c.Flags().Set("params-file", "x.json") },
		"no params at all": func(c *cobra.Command) {},
	} {
		cmd := newDecisionProposeTestCmd()
		_ = cmd.Flags().Set("issue", testIssueUUID)
		_ = cmd.Flags().Set("version", "1")
		set(cmd)
		if _, err := captureStdout(t, func() error { return runDecisionPropose(cmd, []string{"agent.copy"}) }); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if len(posts) != 0 {
		t.Fatalf("a bad invocation reached the server: %v", posts)
	}
}

func TestDecisionProposeShowsTheServerErrorCode(t *testing.T) {
	cases := map[int]string{
		http.StatusServiceUnavailable: "proposals_unavailable: agent action proposals are not enabled on this server",
		http.StatusForbidden:          "proposal_ineligible: recipient cannot approve",
		http.StatusBadRequest:         "target_mismatch: params name another issue",
	}
	for status, msg := range cases {
		var posts []map[string]any
		var other []string
		proposalServer(t, status, map[string]any{"error": msg}, &posts, &other)
		cmd := newDecisionProposeTestCmd()
		_ = cmd.Flags().Set("issue", testIssueUUID)
		_ = cmd.Flags().Set("version", "1")
		_ = cmd.Flags().Set("param", "a=b")
		_, err := captureStdout(t, func() error { return runDecisionPropose(cmd, []string{"agent.copy"}) })
		if err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("status %d: error %v lacks %q", status, err, msg)
		}
	}
}

func TestTaskTokenBudgetRaiseProposesAndPrintsTheCardLink(t *testing.T) {
	var posts []map[string]any
	var other []string
	proposalServer(t, http.StatusCreated, proposalCreated, &posts, &other)
	cmd := newIssueBudgetRaiseTestCmd()
	_ = cmd.Flags().Set("runs", "60")
	_ = cmd.Flags().Set("for", "user-1")
	out, err := captureStdout(t, func() error { return runIssueBudgetRaise(cmd, []string{testIssueUUID}) })
	if err != nil {
		t.Fatalf("raise: %v", err)
	}
	want := map[string]any{
		"action_type": "issue.run_budget.raise", "version": float64(1), "issue_id": testIssueUUID, "for": "user-1",
		"params": map[string]any{"issue_id": testIssueUUID, "window_id": "w-1", "new_cap": float64(60)},
	}
	if len(posts) != 1 || !reflect.DeepEqual(posts[0], want) {
		t.Fatalf("posts = %v, want [%v]", posts, want)
	}
	if len(other) != 0 {
		t.Fatalf("task token used another route: %v", other)
	}
	if !strings.Contains(out, "/acme/issues/"+testIssueUUID+"?approve=act-1") {
		t.Fatalf("output %q lacks the card link", out)
	}
}

func TestProposalOutputNamesTheDMWhereTheCardLanded(t *testing.T) {
	var posts []map[string]any
	var other []string
	proposalServer(t, http.StatusCreated, map[string]any{"id": "req-1", "chat_session_id": "dm-9", "action": map[string]any{"id": "act-1"}}, &posts, &other)
	cmd := newIssueBudgetRaiseTestCmd()
	_ = cmd.Flags().Set("runs", "60")
	_ = cmd.Flags().Set("for", "user-1")
	out, err := captureStdout(t, func() error { return runIssueBudgetRaise(cmd, []string{testIssueUUID}) })
	if err != nil {
		t.Fatalf("raise: %v", err)
	}
	if !strings.Contains(out, "/acme/chat?session=dm-9") {
		t.Fatalf("output %q lacks the DM link", out)
	}
}

func TestTaskTokenBudgetTopUpProposesWithoutNewCap(t *testing.T) {
	var posts []map[string]any
	var other []string
	proposalServer(t, http.StatusCreated, proposalCreated, &posts, &other)
	if _, err := captureStdout(t, func() error { return runIssueBudgetTopUp(newIssueBudgetRaiseTestCmd(), []string{testIssueUUID}) }); err != nil {
		t.Fatalf("top-up: %v", err)
	}
	want := map[string]any{"issue_id": testIssueUUID, "window_id": "w-1"}
	if len(posts) != 1 || posts[0]["action_type"] != "issue.run_budget.top_up" || !reflect.DeepEqual(posts[0]["params"], want) {
		t.Fatalf("posts = %v, want top_up with params %v", posts, want)
	}
}

// asPAT makes the CLI a human PAT caller: no task markers, a non-mat_ token.
// Call it after the server helper, which sets a task token.
func asPAT(t *testing.T) {
	t.Helper()
	clearAmbientDaemonTaskEnv(t)
	t.Setenv("MULTICA_TOKEN", "mul_test-pat")
}
