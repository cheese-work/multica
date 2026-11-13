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

func newIssueBudgetRaiseTestCmd() *cobra.Command {
	c := &cobra.Command{Use: "raise"}
	c.Flags().Int("runs", 0, "")
	c.Flags().String("output", "table", "")
	c.Flags().String("for", "", "")
	return c
}

// budgetRequestServer serves the budget snapshot and records the request
// POSTs, answering each with one approve link.
func budgetRequestServer(t *testing.T, posts *[]map[string]any) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/issues/"+testIssueUUID+"/run-budget":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"budgeted": true, "window_id": "2026-11-01T05:22:39.123456Z", "runs_used": 45, "runs_limit": 40,
				"raise_preview": map[string]any{"limit_after": 55},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/issues/"+testIssueUUID+"/run-budget/requests":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			*posts = append(*posts, body)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"request_id": "req-1", "action_id": "act-1", "approve_url": "/acme/issues/" + testIssueUUID + "?approve=act-1",
				"requests": []map[string]any{{"request_id": "req-1", "action_id": "act-1", "approve_url": "/acme/issues/" + testIssueUUID + "?approve=act-1"}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
}

func TestRunIssueBudgetRaiseRequestsAndPrintsTheApproveURL(t *testing.T) {
	var posts []map[string]any
	budgetRequestServer(t, &posts)
	asPAT(t)
	cmd := newIssueBudgetRaiseTestCmd()
	_ = cmd.Flags().Set("runs", "60")
	out, err := captureStdout(t, func() error { return runIssueBudgetRaise(cmd, []string{testIssueUUID}) })
	if err != nil {
		t.Fatalf("raise: %v", err)
	}
	want := map[string]any{"kind": "raise", "new_cap": float64(60), "window_id": "2026-11-01T05:22:39.123456Z"}
	if len(posts) != 1 || !reflect.DeepEqual(posts[0], want) {
		t.Fatalf("posts = %v, want [%v]", posts, want)
	}
	if !strings.Contains(out, "/acme/issues/"+testIssueUUID+"?approve=act-1") {
		t.Fatalf("output %q lacks the approve URL", out)
	}
}

func TestRunIssueBudgetRaiseDefaultsToTheSuggestedCap(t *testing.T) {
	var posts []map[string]any
	budgetRequestServer(t, &posts)
	asPAT(t)
	if _, err := captureStdout(t, func() error { return runIssueBudgetRaise(newIssueBudgetRaiseTestCmd(), []string{testIssueUUID}) }); err != nil {
		t.Fatalf("raise: %v", err)
	}
	if len(posts) != 1 || posts[0]["new_cap"] != float64(55) {
		t.Fatalf("posts = %v, want new_cap 55 from the raise preview", posts)
	}
}

func TestRunIssueBudgetGetShowsUsedLimitAndRaise(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gotPath = r.URL.Path
			_ = json.NewEncoder(w).Encode(map[string]any{
				"budgeted": true, "runs_used": 45, "runs_limit": 40, "runs_state": "over", "state": "over",
				"blocking": []string{"runs"}, "can_raise": true,
				"raise_preview": map[string]any{"add_runs": 10, "balance_after": 10, "limit_after": 55, "max_allowed_add": 955},
			})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "mat_test-token")

	cmd := &cobra.Command{Use: "get"}
	cmd.Flags().String("output", "table", "")
	out, err := captureStdout(t, func() error { return runIssueBudgetGet(cmd, []string{testIssueUUID}) })
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if gotPath != "/api/issues/"+testIssueUUID+"/run-budget" {
		t.Fatalf("path = %q", gotPath)
	}
	for _, want := range []string{"45/40", "over", "approve a raise to 55 runs in the web app", "10 left"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output %q lacks %q", out, want)
		}
	}
}

func TestRunIssueBudgetGetSaysWhenTheIssueIsNotBudgeted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"budgeted": false, "blocking": []string{}})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "mat_test-token")

	cmd := &cobra.Command{Use: "get"}
	cmd.Flags().String("output", "table", "")
	out, err := captureStdout(t, func() error { return runIssueBudgetGet(cmd, []string{testIssueUUID}) })
	if err != nil || !strings.Contains(out, "no run budget") {
		t.Fatalf("out = %q err = %v", out, err)
	}
}

func TestRunIssueBudgetGetJSONKeepsEveryServerField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"budgeted": true, "runs_source": "issue", "runs_used": 1, "runs_limit": 2})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "mat_test-token")

	cmd := &cobra.Command{Use: "get"}
	cmd.Flags().String("output", "json", "")
	out, err := captureStdout(t, func() error { return runIssueBudgetGet(cmd, []string{testIssueUUID}) })
	if err != nil || !strings.Contains(out, "runs_source") {
		t.Fatalf("out = %q err = %v", out, err)
	}
}

func TestRunIssueBudgetTopUpRequestsWithoutACap(t *testing.T) {
	var posts []map[string]any
	budgetRequestServer(t, &posts)
	asPAT(t)
	cmd := &cobra.Command{Use: "top-up"}
	cmd.Flags().String("output", "table", "")
	out, err := captureStdout(t, func() error { return runIssueBudgetTopUp(cmd, []string{testIssueUUID}) })
	if err != nil {
		t.Fatalf("top-up: %v", err)
	}
	want := map[string]any{"kind": "top_up", "window_id": "2026-11-01T05:22:39.123456Z"}
	if len(posts) != 1 || !reflect.DeepEqual(posts[0], want) {
		t.Fatalf("posts = %v, want [%v]", posts, want)
	}
	if !strings.Contains(out, "?approve=act-1") {
		t.Fatalf("output %q lacks the approve URL", out)
	}
}

// CHE-5503: the budget read names the one next step.
func TestRunIssueBudgetGetPrintsTheNextCommand(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"budgeted": true, "runs_used": 25, "runs_limit": 25, "state": "over",
			"open_card":    map[string]any{"request_id": "req-1", "action_id": "act-1", "url": "/acme/issues/x?approve=act-1"},
			"next_command": "Wait for the approval card /acme/issues/x?approve=act-1. Do not open another.",
		})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "mat_test-token")

	cmd := &cobra.Command{Use: "get"}
	cmd.Flags().String("output", "table", "")
	out, err := captureStdout(t, func() error { return runIssueBudgetGet(cmd, []string{testIssueUUID}) })
	if err != nil || !strings.Contains(out, "Wait for the approval card /acme/issues/x?approve=act-1. Do not open another.") {
		t.Fatalf("out = %q err = %v", out, err)
	}
}

func TestBudgetTopUpIsHiddenFromAgentHelp(t *testing.T) {
	t.Cleanup(hideMemberOnlyBudgetCommands)
	for token, hidden := range map[string]bool{"mat_task-token": true, "mul_member-pat": false} {
		t.Setenv("MULTICA_TOKEN", token)
		hideMemberOnlyBudgetCommands()
		if got := !issueBudgetTopUpCmd.IsAvailableCommand(); got != hidden {
			t.Fatalf("token %q: top-up hidden = %v, want %v", token, got, hidden)
		}
		if !issueBudgetRaiseCmd.IsAvailableCommand() {
			t.Fatalf("token %q: raise must stay listed", token)
		}
	}
}
