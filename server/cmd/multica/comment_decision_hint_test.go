package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestIssueCommentAddDecisionHint(test *testing.T) {
	const issueID = "11111111-1111-4111-8111-111111111111"
	const memberID = "22222222-2222-4222-8222-222222222222"
	const hint = "For an answer, prefer multica decision request --issue " + issueID + " --for " + memberID
	for _, output := range []string{"json", "table"} {
		for _, advisory := range []bool{false, true} {
			name := output + map[bool]string{false: "/no_hint", true: "/hint"}[advisory]
			test.Run(name, func(test *testing.T) {
				test.Chdir(test.TempDir())
				posted := 0
				response := map[string]any{"id": "saved-comment", "content": "[@Human](mention://member/" + memberID + ")"}
				if advisory {
					response["hints"] = []any{hint}
				}
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if request.Method == http.MethodGet {
						_ = json.NewEncoder(writer).Encode(map[string]any{"id": issueID, "identifier": "TST-1"})
						return
					}
					posted++
					var body map[string]any
					if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
						test.Error(err)
					}
					if body["content"] != response["content"] {
						test.Errorf("comment content changed: %v", body)
					}
					writer.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(writer).Encode(response)
				}))
				defer server.Close()
				setCLITestServerEnv(test, server.URL)
				test.Setenv("MULTICA_TOKEN", "mat_test-token")
				command := newIssueCommentAddTestCmd()
				_ = command.Flags().Set("content", response["content"].(string))
				_ = command.Flags().Set("output", output)
				stderr := captureStderr(test)
				stdout, err := captureStdout(test, func() error { return runIssueCommentAdd(command, []string{issueID}) })
				warning := stderr.read()
				if err != nil || posted != 1 {
					test.Fatalf("comment must post once: posted=%d err=%v", posted, err)
				}
				if count := strings.Count(warning, hint); count != map[bool]int{false: 0, true: 1}[advisory] {
					test.Errorf("stderr hint count=%d: %s", count, warning)
				}
				if output == "table" {
					if stdout != "" {
						test.Errorf("table stdout must stay empty: %q", stdout)
					}
				} else {
					var decoded map[string]any
					if err := json.Unmarshal([]byte(stdout), &decoded); err != nil || !reflect.DeepEqual(decoded, response) {
						test.Errorf("JSON stdout must preserve the API response: %q, err=%v", stdout, err)
					}
				}
			})
		}
	}
}
