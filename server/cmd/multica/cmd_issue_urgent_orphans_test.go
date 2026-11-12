package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func urgentOrphansTestCmd(output string) *cobra.Command {
	c := &cobra.Command{Use: "urgent-orphans"}
	c.Flags().String("output", output, "")
	return c
}

func urgentOrphansServer(t *testing.T) *string {
	t.Helper()
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{"issues": []map[string]any{
			{"id": "i-1", "number": 5118, "title": "orphan one", "status": "blocked", "assignee_type": "agent", "assignee_id": "a-1", "updated_at": "2026-11-10T09:00:00Z"},
		}})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
	return &path
}

// The pulse is a CLI-only agent, so the urgent-orphan list must be one
// `multica` command away.
func TestRunIssueUrgentOrphansCallsTheEndpointAndPrintsJSON(t *testing.T) {
	path := urgentOrphansServer(t)
	out, err := captureStdout(t, func() error { return runIssueUrgentOrphans(urgentOrphansTestCmd("json"), nil) })
	if err != nil {
		t.Fatalf("urgent-orphans: %v", err)
	}
	if *path != "/api/issues/urgent-orphans" {
		t.Fatalf("path = %q, want /api/issues/urgent-orphans", *path)
	}
	var resp struct {
		Issues []struct {
			Number int `json:"number"`
		} `json:"issues"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil || len(resp.Issues) != 1 || resp.Issues[0].Number != 5118 {
		t.Fatalf("json output = %q (err %v)", out, err)
	}
}

func TestRunIssueUrgentOrphansTableListsTheIssue(t *testing.T) {
	urgentOrphansServer(t)
	out, err := captureStdout(t, func() error { return runIssueUrgentOrphans(urgentOrphansTestCmd("table"), nil) })
	if err != nil {
		t.Fatalf("urgent-orphans: %v", err)
	}
	if !strings.Contains(out, "orphan one") || !strings.Contains(out, "blocked") {
		t.Fatalf("table output = %q", out)
	}
}
