package main

import (
	"encoding/json"
	"io"
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

// budgetServer answers every POST with status/body and records it.
func budgetServer(t *testing.T, status int, body any, got *struct{ Path, Body string }) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			b, _ := io.ReadAll(r.Body)
			got.Path, got.Body = r.URL.Path, strings.TrimSpace(string(b))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(body)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
}

func TestRunIssueBudgetRaisePostsTheTotal(t *testing.T) {
	var got struct{ Path, Body string }
	budgetServer(t, 200, map[string]any{"max_runs": 20, "previous_max_runs": 10}, &got)
	cmd := newIssueBudgetRaiseTestCmd()
	_ = cmd.Flags().Set("runs", "20")
	out, err := captureStdout(t, func() error { return runIssueBudgetRaise(cmd, []string{testIssueUUID}) })
	if err != nil {
		t.Fatalf("raise: %v", err)
	}
	if got.Path != "/api/issues/"+testIssueUUID+"/run-budget" || got.Body != `{"runs":20}` {
		t.Fatalf("sent %s %s", got.Path, got.Body)
	}
	if !strings.Contains(out, "from 10 to 20") {
		t.Fatalf("out = %q", out)
	}
}

func TestRunIssueBudgetRaiseNeedsRuns(t *testing.T) {
	var got struct{ Path, Body string }
	budgetServer(t, 200, map[string]any{}, &got)
	if err := runIssueBudgetRaise(newIssueBudgetRaiseTestCmd(), []string{testIssueUUID}); err == nil || got.Path != "" {
		t.Fatalf("err = %v, sent %q; want a local error and no request", err, got.Path)
	}
}

func TestRunIssueBudgetRaiseExplainsACapRefusal(t *testing.T) {
	var got struct{ Path, Body string }
	budgetServer(t, 429, map[string]any{
		"error": "agent run-budget cap reached", "reason": "daily", "limit_usd": 10.0, "used_usd": 9.5,
		"remaining_usd": 0.5, "remaining_runs": 2, "requested_runs": 5, "resets_at": "2026-11-07T00:00:00+07:00",
	}, &got)
	cmd := newIssueBudgetRaiseTestCmd()
	_ = cmd.Flags().Set("runs", "20")
	err := runIssueBudgetRaise(cmd, []string{testIssueUUID})
	if err == nil {
		t.Fatal("want a refusal")
	}
	for _, want := range []string{"agent run-budget cap reached", "daily", "$0.50 left (2 runs)", "asked for 5 runs", "resets 2026-11-07T00:00:00+07:00"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err %q lacks %q", err, want)
		}
	}
}

func TestRunIssueBudgetTopUpPostsAndReports(t *testing.T) {
	var got struct{ Path, Body string }
	budgetServer(t, 200, map[string]any{"max_runs": 10}, &got)
	cmd := &cobra.Command{Use: "top-up"}
	cmd.Flags().String("output", "table", "")
	out, err := captureStdout(t, func() error { return runIssueBudgetTopUp(cmd, []string{testIssueUUID}) })
	if err != nil {
		t.Fatalf("top-up: %v", err)
	}
	if got.Path != "/api/issues/"+testIssueUUID+"/run-budget/top-up" || !strings.Contains(out, "10 runs") {
		t.Fatalf("sent %s, out %q", got.Path, out)
	}
}

func TestRunIssueBudgetTopUpReportsADisabledAgentCap(t *testing.T) {
	var got struct{ Path, Body string }
	budgetServer(t, 403, map[string]any{"error": "agents cannot raise a run budget on this server", "reason": "disabled"}, &got)
	cmd := &cobra.Command{Use: "top-up"}
	cmd.Flags().String("output", "table", "")
	err := runIssueBudgetTopUp(cmd, []string{testIssueUUID})
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("err = %v, want the disabled reason", err)
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
