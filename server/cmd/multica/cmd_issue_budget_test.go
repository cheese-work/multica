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
