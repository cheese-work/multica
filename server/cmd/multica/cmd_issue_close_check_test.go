package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIssueCloseCheckCommandReplaysOneActivityRow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/issues/CHE-1":
			w.Write([]byte(`{"id":"` + testIssueUUID + `","identifier":"CHE-1"}`))
		case "/api/issues/" + testIssueUUID + "/close-check":
			if r.URL.Query().Get("activity") != "row-1" {
				t.Errorf("activity query = %q, want row-1", r.URL.RawQuery)
			}
			w.Write([]byte(`{"source":"activity","activity_id":"row-1","checked_at":"2026-11-23T05:00:00Z","mode":{"value":"block","source":"workspace"},
				"outcome":"rejected","first_failure":"post_close_work_none","recorded_first_failure":"post_close_work_none",
				"predicates":[{"name":"done_edge","read":true,"held":true,"value":"agent_done=true pr_merged=false"},
				{"name":"post_close_work_none","read":true,"held":false,"value":"post_close_work=\"\""}]}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	cmd, _, err := rootCmd.Find([]string{"issue", "close-check"})
	if err != nil || cmd.Name() != "close-check" {
		t.Fatal("issue close-check is unavailable")
	}
	if err := cmd.Flags().Set("activity", "row-1"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Flags().Set("activity", "") })
	out, err := captureStdout(t, func() error { return cmd.RunE(cmd, []string{"CHE-1"}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"mode: block (from workspace)", "FAIL post_close_work_none", "first failure: post_close_work_none", "ok   done_edge"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}
