package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIssueCommentDecisionIntentFlag(test *testing.T) {
	flag := issueCommentAddCmd.Flags().Lookup("decision-intent")
	if flag == nil {
		test.Fatal("comment add lacks --decision-intent")
	}
	if !strings.Contains(flag.Usage, "running non-chat issue tasks only") {
		test.Fatalf("decision-intent help = %q, want running non-chat issue tasks only", flag.Usage)
	}
}

func TestIssueCommentDecisionIntentBody(test *testing.T) {
	const issueID = "11111111-1111-4111-8111-111111111111"
	for _, enabled := range []bool{false, true} {
		test.Run(map[bool]string{false: "ordinary", true: "intent"}[enabled], func(test *testing.T) {
			var body map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet {
					_ = json.NewEncoder(writer).Encode(map[string]any{"id": issueID, "identifier": "TST-1"})
					return
				}
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					test.Error(err)
				}
				_ = json.NewEncoder(writer).Encode(map[string]any{"id": issueID})
			}))
			defer server.Close()
			test.Setenv("MULTICA_SERVER_URL", server.URL)
			test.Setenv("MULTICA_TOKEN", "mat_test-token")
			test.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
			command := newIssueCommentAddTestCmd()
			command.Flags().Bool("decision-intent", enabled, "")
			_ = command.Flags().Set("content", "ask a human")
			_, err := captureStdout(test, func() error { return runIssueCommentAdd(command, []string{issueID}) })
			if err != nil {
				test.Fatal(err)
			}
			value, present := body["decision_intent"]
			if present != enabled || (enabled && value != true) {
				test.Fatalf("decision_intent = %v, present=%t, enabled=%t", value, present, enabled)
			}
		})
	}
}
