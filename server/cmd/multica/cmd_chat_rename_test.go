package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestChatRenameCommand(test *testing.T) {
	command, _, err := chatCmd.Find([]string{"rename"})
	if err != nil || command == chatCmd {
		test.Fatal("chat rename command is missing")
	}
	if err := command.Args(command, []string{"session-id"}); err == nil {
		test.Fatal("rename accepted a missing title")
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPatch || request.URL.Path != "/api/chat/sessions/session-id" {
			test.Errorf("unexpected request: %s %s", request.Method, request.URL)
		}
		var body map[string]string
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body["title"] != "Decision lane" || len(body) != 1 {
			test.Errorf("rename body = %v, error = %v", body, err)
		}
		_ = json.NewEncoder(writer).Encode(map[string]string{"id": "session-id", "title": "Decision lane", "previous_title": "Old lane", "message_id": "notice-id"})
	}))
	defer server.Close()
	test.Setenv("MULTICA_SERVER_URL", server.URL)
	test.Setenv("MULTICA_TOKEN", "mat_test")
	test.Setenv("MULTICA_WORKSPACE_ID", "workspace-id")
	output, err := captureStdout(test, func() error { return command.RunE(command, []string{"session-id", "Decision lane"}) })
	if err != nil {
		test.Fatal(err)
	}
	var result map[string]string
	if err := json.Unmarshal([]byte(output), &result); err != nil || result["title"] != "Decision lane" || result["message_id"] != "notice-id" {
		test.Fatalf("rename output = %s, error = %v", output, err)
	}
}
