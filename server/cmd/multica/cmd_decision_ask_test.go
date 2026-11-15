package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecisionAskPostsOnlyTheAskToTheOneStepRoute(test *testing.T) {
	for _, words := range [][]string{{"decision", "ask"}, {"agent", "decision", "ask"}} {
		test.Run(strings.Join(words, " "), func(test *testing.T) { postDecisionAsk(test, words) })
	}
}

func postDecisionAsk(test *testing.T, words []string) {
	var path string
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		path = request.URL.Path
		_ = json.NewDecoder(request.Body).Decode(&body)
		_ = json.NewEncoder(writer).Encode(map[string]any{"id": "11111111-1111-4111-8111-111111111111"})
	}))
	defer server.Close()
	test.Setenv("MULTICA_SERVER_URL", server.URL)
	test.Setenv("MULTICA_TOKEN", "mat_test-token")
	test.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	command, _, err := rootCmd.Find(words)
	if err != nil || command.Name() != "ask" {
		test.Fatalf("%v is unavailable: %v", words, err)
	}
	askPath := filepath.Join(test.TempDir(), "ask.json")
	if err := os.WriteFile(askPath, []byte(structuredAskJSON(test, "Ship it?")), 0o600); err != nil {
		test.Fatal(err)
	}
	for name, value := range map[string]string{"ask-file": askPath, "class": "reversible"} {
		if err := command.Flags().Set(name, value); err != nil {
			test.Fatal(err)
		}
	}
	if _, err := captureStdout(test, func() error { return command.RunE(command, nil) }); err != nil {
		test.Fatal(err)
	}
	if path != "/api/decisions/ask" || body["escalation_class"] != "reversible" || body["issue_id"] != nil || body["for"] != nil || body["ask"] == nil {
		test.Fatalf("POST %s body %v, want only ask, footer, parent_id and escalation_class", path, body)
	}
}
