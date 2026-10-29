package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatSessionsWindowCommand(test *testing.T) {
	command, _, err := chatCmd.Find([]string{"sessions"})
	if err != nil || command == chatCmd || command.Flags().Lookup("issue") == nil || command.Flags().Lookup("for") == nil {
		test.Fatal("chat sessions lacks the explicit --issue/--for window path")
	}
	for _, target := range []string{"", "human-id"} {
		test.Run("target="+target, func(test *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodGet || request.URL.Path != "/api/chat/sessions" || request.URL.Query().Get("issue") != "CHE-1559" || request.URL.Query().Get("for") != target {
					test.Errorf("unexpected request: %s %s", request.Method, request.URL)
				}
				_ = json.NewEncoder(writer).Encode([]map[string]any{{"id": "lane-id", "eligible": true, "title": "Decisions", "issue_mentions": []map[string]any{{"issue_key": "CHE-1611", "issue_title": "Release", "relative_time": "2m ago", "snippet": "Choose this lane"}}}})
			}))
			defer server.Close()
			test.Setenv("MULTICA_SERVER_URL", server.URL)
			test.Setenv("MULTICA_TOKEN", "mat_test-token")
			test.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
			_ = command.Flags().Set("issue", "CHE-1559")
			_ = command.Flags().Set("for", target)
			command.Flags().Lookup("for").Changed = target != ""
			_ = command.Flags().Set("output", "json")
			output, err := captureStdout(test, func() error { return command.RunE(command, nil) })
			if err != nil {
				test.Fatal(err)
			}
			var lanes []map[string]any
			if err := json.Unmarshal([]byte(output), &lanes); err != nil || len(lanes) != 1 || lanes[0]["id"] != "lane-id" {
				test.Fatalf("wrong output: %s error=%v", output, err)
			}
			_ = command.Flags().Set("output", "table")
			output, err = captureStdout(test, func() error { return command.RunE(command, nil) })
			if err != nil || !strings.Contains(output, "CHE-1611") || !strings.Contains(output, "Choose this lane") || !strings.Contains(output, "ELIGIBLE") || !strings.Contains(output, "yes") || strings.Contains(output, "LANE_SUMMARY") {
				test.Fatalf("wrong table output: %s error=%v", output, err)
			}
		})
	}
	_ = command.Flags().Set("issue", "")
	if err := command.RunE(command, nil); err == nil {
		test.Fatal("lane listing without --issue was allowed")
	}
}
