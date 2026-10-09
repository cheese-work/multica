package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// CHE-1418 acceptance 6: a trigger the server refused (for example a squad
// mention suppressed as the author's own) is printed as a visible warning,
// whatever the output format.
func TestIssueCommentAddWarnsOnBlockedTriggers_CHE1418(test *testing.T) {
	const issueID = "11111111-1111-4111-8111-111111111111"
	const squadID = "22222222-2222-4222-8222-222222222222"
	const agentID = "33333333-3333-4333-8333-333333333333"
	for _, output := range []string{"json", "table"} {
		test.Run(output, func(test *testing.T) {
			test.Chdir(test.TempDir())
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				if request.Method != http.MethodPost {
					_ = json.NewEncoder(writer).Encode(map[string]any{"id": issueID, "identifier": "CHE-1"})
					return
				}
				writer.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(writer).Encode(map[string]any{
					"id": "44444444-4444-4444-8444-444444444444",
					"trigger_outcomes": []map[string]any{
						{"target_type": "squad", "target_id": squadID, "status": "blocked", "reason_code": "self_trigger_suppressed"},
						{"target_type": "agent", "target_id": agentID, "status": "queued", "reason_code": "queued"},
					},
				})
			}))
			defer server.Close()
			setCLITestServerEnv(test, server.URL)
			test.Setenv("MULTICA_TOKEN", "mat_test-token")
			command := newIssueCommentAddTestCmd()
			for flag, value := range map[string]string{"content": "Return assignee: dev team", "output": output} {
				if err := command.Flags().Set(flag, value); err != nil {
					test.Fatal(err)
				}
			}
			stderr := captureStderr(test)
			err := runIssueCommentAdd(command, []string{issueID})
			got := stderr.read()
			if err != nil {
				test.Fatalf("runIssueCommentAdd: %v", err)
			}
			if !strings.Contains(got, "squad "+squadID) || !strings.Contains(got, "self_trigger_suppressed") {
				test.Fatalf("missing warning for the blocked squad trigger:\n%s", got)
			}
			if strings.Contains(got, agentID) {
				test.Fatalf("a queued trigger must not warn:\n%s", got)
			}
		})
	}
}
