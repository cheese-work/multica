package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func newIssueBudgetRaiseTestCmd() *cobra.Command {
	c := &cobra.Command{Use: "raise"}
	c.Flags().Int("runs", 0, "")
	c.Flags().String("output", "table", "")
	return c
}

func TestRunIssueBudgetRaisePostsRunsToTheIssue(t *testing.T) {
	var gotBody map[string]any
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			gotPath = r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"max_runs": 20, "previous_max_runs": 15})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "mat_test-token")

	cmd := newIssueBudgetRaiseTestCmd()
	_ = cmd.Flags().Set("runs", "20")
	out, err := captureStdout(t, func() error { return runIssueBudgetRaise(cmd, []string{testIssueUUID}) })
	if err != nil {
		t.Fatalf("raise: %v", err)
	}
	if gotPath != "/api/issues/"+testIssueUUID+"/run-budget" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotBody["runs"] != float64(20) {
		t.Fatalf("body = %v, want runs=20", gotBody)
	}
	if !strings.Contains(out, "15") || !strings.Contains(out, "20") {
		t.Fatalf("output %q lacks the old and new cap", out)
	}
}

func TestRunIssueBudgetRaiseRequiresRuns(t *testing.T) {
	cmd := newIssueBudgetRaiseTestCmd()
	if err := runIssueBudgetRaise(cmd, []string{testIssueUUID}); err == nil {
		t.Fatal("raise without --runs succeeded")
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
	for _, want := range []string{"45/40", "over", "--runs 55", "10 left"} {
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

func TestRunIssueBudgetTopUpPostsToTheIssue(t *testing.T) {
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		_ = json.NewEncoder(w).Encode(map[string]any{"max_runs": 40})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "mat_test-token")

	cmd := &cobra.Command{Use: "top-up"}
	cmd.Flags().String("output", "table", "")
	out, err := captureStdout(t, func() error { return runIssueBudgetTopUp(cmd, []string{testIssueUUID}) })
	if err != nil {
		t.Fatalf("top-up: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/issues/"+testIssueUUID+"/run-budget/top-up" {
		t.Fatalf("request = %s %s", gotMethod, gotPath)
	}
	if !strings.Contains(out, "40") {
		t.Fatalf("output %q lacks the new window's runs", out)
	}
}
