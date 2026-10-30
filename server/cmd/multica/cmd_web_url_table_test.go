package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const tableWebURL = "https://app.example.test/ws/issues/MUL-1"

// CHE-2572: table output surfaces the server's web_url when present and never
// invents one when absent.
func TestIssueTableOutputsPrintServerWebURL(t *testing.T) {
	const issueID = "11111111-1111-4111-8111-111111111111"
	for name, withWebURL := range map[string]bool{"present": true, "absent": false} {
		t.Run(name, func(t *testing.T) {
			respond := func(w http.ResponseWriter, object map[string]any, webURL string) {
				if withWebURL {
					object["web_url"] = webURL
				}
				_ = json.NewEncoder(w).Encode(object)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/issues/"+issueID:
					respond(w, map[string]any{"id": issueID, "identifier": "MUL-1", "title": "T", "status": "todo"}, tableWebURL)
				case r.Method == http.MethodPost && r.URL.Path == "/api/issues":
					respond(w, map[string]any{"id": issueID, "identifier": "MUL-1", "title": "T", "status": "todo", "priority": "none"}, tableWebURL)
				case r.Method == http.MethodPost && r.URL.Path == "/api/issues/"+issueID+"/comments":
					respond(w, map[string]any{"id": "comment-1"}, tableWebURL+"#comment-comment-1")
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			setCLITestServerEnv(t, srv.URL)
			t.Setenv("MULTICA_TOKEN", "mat_test-token")

			get := func() error { return runIssueGet(newIssueGetTestCmd("table", false), []string{issueID}) }
			create := func() error {
				cmd := newIssueCreateTestCmd()
				_ = cmd.Flags().Set("title", "T")
				_ = cmd.Flags().Set("output", "table")
				return runIssueCreate(cmd, nil)
			}
			comment := func() error {
				cmd := newIssueCommentAddTestCmd()
				_ = cmd.Flags().Set("content", "hello")
				_ = cmd.Flags().Set("output", "table")
				return runIssueCommentAdd(cmd, []string{issueID})
			}
			for label, run := range map[string]struct {
				fn   func() error
				want string
			}{
				"issue get":    {get, tableWebURL},
				"issue create": {create, tableWebURL},
				"comment add":  {comment, tableWebURL + "#comment-comment-1"},
			} {
				out, err := captureStdout(t, run.fn)
				if err != nil {
					t.Fatalf("%s: %v", label, err)
				}
				if got := strings.Contains(out, run.want); got != withWebURL {
					t.Errorf("%s: output contains web_url = %v, want %v\n%s", label, got, withWebURL, out)
				}
				if !withWebURL && strings.Contains(out, "https://") {
					t.Errorf("%s: must not print an origin the server did not send\n%s", label, out)
				}
			}
		})
	}
}
