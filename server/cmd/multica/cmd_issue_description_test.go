package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func descriptionTestServer(t *testing.T, handler http.HandlerFunc) *[]string {
	t.Helper()
	var urls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/issues/"+testIssueUUID:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": testIssueUUID, "identifier": "MUL-1", "title": "t"})
		case strings.HasPrefix(r.URL.Path, "/api/issues/"+testIssueUUID+"/description/"):
			urls = append(urls, r.URL.RequestURI())
			handler(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
	return &urls
}

func TestRunIssueDescriptionHistoryPrintsIctTimeAndActor(t *testing.T) {
	descriptionTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"revisions": []map[string]any{
			{"revision": 3, "actor_type": "member", "actor_id": "u-1", "created_at": "2026-11-10T17:30:00Z"},
			{"revision": 7, "actor_type": nil, "actor_id": nil, "created_at": "2026-11-11T01:00:00Z"},
		}})
	})
	cmd := &cobra.Command{Use: "history"}
	cmd.Flags().String("output", "table", "")

	out, err := captureStdout(t, func() error { return runIssueDescriptionHistory(cmd, []string{testIssueUUID}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"REVISION", "2026-11-11 00:30 ICT", "member:u-1", "2026-11-11 08:00 ICT"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "\n3 ") > strings.Index(out, "\n7 ") {
		t.Errorf("revisions out of order:\n%s", out)
	}
}

func TestRunIssueDescriptionDiffPrintsUnifiedDiff(t *testing.T) {
	urls := descriptionTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"from": 3, "to": 7, "diff": "--- revision 3\n+++ revision 7\n@@ -1 +1 @@\n-a\n+b\n"})
	})
	cmd := &cobra.Command{Use: "diff"}
	cmd.Flags().String("output", "table", "")
	cmd.Flags().Int64("from", 0, "")
	cmd.Flags().Int64("to", 0, "")
	_ = cmd.Flags().Set("from", "3")
	_ = cmd.Flags().Set("to", "7")

	out, err := captureStdout(t, func() error { return runIssueDescriptionDiff(cmd, []string{testIssueUUID}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "-a\n+b\n") {
		t.Errorf("diff not printed:\n%s", out)
	}
	if got := *urls; len(got) != 1 || !strings.Contains(got[0], "from=3") || !strings.Contains(got[0], "to=7") {
		t.Errorf("request = %v, want from=3&to=7", got)
	}
}

func TestRunIssueDescriptionDiffRequiresFromAndTo(t *testing.T) {
	cmd := &cobra.Command{Use: "diff"}
	cmd.Flags().String("output", "table", "")
	cmd.Flags().Int64("from", 0, "")
	cmd.Flags().Int64("to", 0, "")
	_ = cmd.Flags().Set("from", "3")
	if err := runIssueDescriptionDiff(cmd, []string{testIssueUUID}); err == nil || !strings.Contains(err.Error(), "--to") {
		t.Fatalf("err = %v, want --to required", err)
	}
}
