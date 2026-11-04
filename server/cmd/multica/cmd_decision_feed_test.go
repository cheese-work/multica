package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDecisionFeedCommands(test *testing.T) {
	var gotPath, gotMethod string
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotPath, gotMethod = request.URL.Path, request.Method
		body = nil
		_ = json.NewDecoder(request.Body).Decode(&body)
		_ = json.NewEncoder(writer).Encode(map[string]any{"answers": []any{}, "applied": true})
	}))
	defer server.Close()
	test.Chdir(test.TempDir())
	test.Setenv("MULTICA_SERVER_URL", server.URL)
	test.Setenv("MULTICA_TOKEN", "mul_fake-sweep-token")
	test.Setenv("MULTICA_WORKSPACE_ID", "ws-1")

	if _, err := captureStdout(test, func() error { return runDecisionFeed(decisionFeedCmd, nil) }); err != nil {
		test.Fatal(err)
	}
	if gotMethod != http.MethodGet || gotPath != "/api/decisions/feed" {
		test.Fatalf("feed called %s %s", gotMethod, gotPath)
	}

	_ = decisionFeedAckCmd.Flags().Set("version", "3")
	if _, err := captureStdout(test, func() error { return runDecisionFeedAck(decisionFeedAckCmd, []string{"abc"}) }); err != nil {
		test.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/decisions/feed/abc/ack" || body["version"] != float64(3) {
		test.Fatalf("ack called %s %s %v", gotMethod, gotPath, body)
	}
	_ = decisionFeedAckCmd.Flags().Set("version", "0")
	if err := runDecisionFeedAck(decisionFeedAckCmd, []string{"abc"}); err == nil {
		test.Fatal("version 0 must be refused")
	}
}
