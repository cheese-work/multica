package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// Natural language never drives a CLI refusal: an agent comment that names a
// member reaches the server whatever its language or shape. The server decides.
func TestAgentCommentWithMemberMentionIsNotRefusedByText(t *testing.T) {
	const member = "[@Alex](mention://member/f0c4c4cf-b6cd-4939-bfba-a5e14290c1ff)"
	for _, tc := range []struct{ name, content string }{
		{"vietnamese ask", member + " vui lòng merge PR #601 giúp mình."},
		{"chinese ask", member + " 请合并 PR #601。"},
		{"english ask without labels", "**Alex:** merge PR #601."},
		{"plain-name ask", "Alex ơi, hãy duyệt PR #601."},
		{"fyi receipt", "Receipt: done. " + member},
	} {
		for _, op := range []string{"add", "update"} {
			t.Run(tc.name+"/"+op, func(t *testing.T) {
				t.Chdir(t.TempDir())
				var writes atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet {
						writes.Add(1)
					}
					w.WriteHeader(http.StatusOK)
					_ = json.NewEncoder(w).Encode(map[string]any{"id": "c1", "identifier": "TST-1"})
				}))
				defer srv.Close()
				setCLITestServerEnv(t, srv.URL)
				t.Setenv("MULTICA_TOKEN", "mat_test-token")
				t.Setenv("MULTICA_AGENT_ID", "abcdef12-1111-4111-8111-111111111111")
				cmd, run := newIssueCommentAddTestCmd(), runIssueCommentAdd
				if op == "update" {
					cmd, run = newIssueCommentUpdateTestCmd(), runIssueCommentUpdate
					if err := cmd.Flags().Set("expected-revision", "1"); err != nil {
						t.Fatal(err)
					}
				}
				if err := cmd.Flags().Set("content", tc.content); err != nil {
					t.Fatal(err)
				}
				err := run(cmd, []string{"11111111-1111-4111-8111-111111111111"})
				if writes.Load() == 0 {
					t.Fatalf("CLI refused before any request: err=%v", err)
				}
			})
		}
	}
}
