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

const budgetWebAppPointer = "approve in the web app; until then move the status in the web app"

// refusingBudgetServer fails the test on any request: a raise or top-up from
// the CLI is a web-app action, so the CLI must not send one (CHE-2798 S1a).
func refusingBudgetServer(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("CLI sent %s %s; a budget raise or top-up is approved in the web app", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
}

func TestRunIssueBudgetRaisePointsToTheWebApp(t *testing.T) {
	refusingBudgetServer(t)
	cmd := newIssueBudgetRaiseTestCmd()
	_ = cmd.Flags().Set("runs", "20")
	err := runIssueBudgetRaise(cmd, []string{testIssueUUID})
	if err == nil || !strings.Contains(err.Error(), budgetWebAppPointer) {
		t.Fatalf("raise err = %v, want the web-app pointer", err)
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

func TestRunIssueBudgetTopUpPointsToTheWebApp(t *testing.T) {
	refusingBudgetServer(t)
	cmd := &cobra.Command{Use: "top-up"}
	cmd.Flags().String("output", "table", "")
	err := runIssueBudgetTopUp(cmd, []string{testIssueUUID})
	if err == nil || !strings.Contains(err.Error(), budgetWebAppPointer) {
		t.Fatalf("top-up err = %v, want the web-app pointer", err)
	}
}
