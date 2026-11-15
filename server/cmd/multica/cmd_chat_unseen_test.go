package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func useChatTestServer(test *testing.T, handler http.HandlerFunc) {
	test.Helper()
	server := httptest.NewServer(handler)
	test.Cleanup(server.Close)
	test.Setenv("MULTICA_SERVER_URL", server.URL)
	test.Setenv("MULTICA_TOKEN", "mat_test-token")
	test.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
}

func TestChatUnseenListsOnlyUnreadDMs(test *testing.T) {
	command, _, err := chatCmd.Find([]string{"unseen"})
	if err != nil || command == chatCmd {
		test.Fatal("chat unseen command is missing")
	}
	useChatTestServer(test, func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/api/chat/sessions" {
			test.Errorf("unexpected request: %s %s", request.Method, request.URL)
		}
		_ = json.NewEncoder(writer).Encode([]map[string]any{
			{"id": "read-dm", "title": "Read", "unread_count": 0, "has_unread": false, "updated_at": "2026-11-05T01:00:00Z"},
			{"id": "unseen-dm", "title": "Unseen", "unread_count": 2, "has_unread": true, "updated_at": "2026-11-05T02:00:00Z"},
		})
	})
	_ = command.Flags().Set("output", "json")
	output, err := captureStdout(test, func() error { return command.RunE(command, nil) })
	if err != nil {
		test.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(output), &rows); err != nil || len(rows) != 1 || rows[0]["id"] != "unseen-dm" || rows[0]["unseen"] != true || rows[0]["unread_count"] != float64(2) {
		test.Fatalf("wrong json output: %s error=%v", output, err)
	}
	_ = command.Flags().Set("output", "table")
	output, err = captureStdout(test, func() error { return command.RunE(command, nil) })
	if err != nil || !strings.Contains(output, "unseen-dm") || strings.Contains(output, "read-dm") || !strings.Contains(output, "UNREAD") {
		test.Fatalf("wrong table output: %s error=%v", output, err)
	}
}

func TestChatMarkUnseenPostsUnread(test *testing.T) {
	command, _, err := chatCmd.Find([]string{"mark-unseen"})
	if err != nil || command == chatCmd {
		test.Fatal("chat mark-unseen command is missing")
	}
	var called bool
	useChatTestServer(test, func(writer http.ResponseWriter, request *http.Request) {
		called = true
		if request.Method != http.MethodPost || request.URL.Path != "/api/chat/sessions/dm-1/unread" {
			test.Errorf("unexpected request: %s %s", request.Method, request.URL)
		}
		writer.WriteHeader(http.StatusNoContent)
	})
	output, err := captureStdout(test, func() error { return command.RunE(command, []string{"dm-1"}) })
	if err != nil || !called || !strings.Contains(output, "dm-1") {
		test.Fatalf("mark-unseen failed: called=%v output=%q error=%v", called, output, err)
	}
}

func TestChatMarkUnseenSurfacesServerError(test *testing.T) {
	command, _, _ := chatCmd.Find([]string{"mark-unseen"})
	if command == nil || command == chatCmd {
		test.Fatal("chat mark-unseen command is missing")
	}
	useChatTestServer(test, func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, `{"error":"chat session not found"}`, http.StatusNotFound)
	})
	if _, err := captureStdout(test, func() error { return command.RunE(command, []string{"missing"}) }); err == nil || !strings.Contains(err.Error(), "mark chat session unseen") {
		test.Fatalf("server error was swallowed: %v", err)
	}
}
