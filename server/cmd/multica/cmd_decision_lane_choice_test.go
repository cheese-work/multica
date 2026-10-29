package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestDecisionRequestNewDMCommand(test *testing.T) {
	command := decisionRequestCmd
	for _, name := range []string{"issue", "session", "for", "new-dm", "class", "ask-file"} {
		flag := command.Flags().Lookup(name)
		if flag == nil {
			test.Fatalf("missing flag %s", name)
		}
		original := flag.Value.String()
		test.Cleanup(func() { _ = command.Flags().Set(name, original) })
	}
	askPath := filepath.Join(test.TempDir(), "ask.txt")
	if err := os.WriteFile(askPath, []byte(structuredAskJSON(test, "Pick a new DM")), 0600); err != nil {
		test.Fatal(err)
	}
	posted := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/decisions" {
			_ = json.NewEncoder(writer).Encode(map[string]any{"id": "11111111-1111-4111-8111-111111111111"})
			return
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			test.Error(err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if body["new_dm"] != true || body["for"] != "human-id" || body["session_id"] != "" {
			test.Errorf("wrong explicit new-DM body: %+v", body)
		}
		posted = true
		_ = json.NewEncoder(writer).Encode(map[string]any{"id": "request-id"})
	}))
	defer server.Close()
	test.Setenv("MULTICA_SERVER_URL", server.URL)
	test.Setenv("MULTICA_TOKEN", "mat_test-token")
	test.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	for name, value := range map[string]string{"issue": "11111111-1111-4111-8111-111111111111", "session": "", "new-dm": "true", "for": "human-id", "class": "reversible", "ask-file": askPath} {
		_ = command.Flags().Set(name, value)
	}
	if _, err := captureStdout(test, func() error { return runDecisionRequest(command, nil) }); err != nil || !posted {
		test.Fatalf("new-DM command: posted=%t error=%v", posted, err)
	}
	for _, scenario := range []string{"both", "missing_person", "implicit"} {
		_ = command.Flags().Set("session", "")
		_ = command.Flags().Set("new-dm", "true")
		_ = command.Flags().Set("for", "human-id")
		switch scenario {
		case "both":
			_ = command.Flags().Set("session", "lane-id")
		case "missing_person":
			_ = command.Flags().Set("for", "")
		case "implicit":
			_ = command.Flags().Set("new-dm", "false")
			_ = command.Flags().Set("for", "")
		}
		if err := runDecisionRequest(command, nil); err == nil {
			test.Fatalf("accepted %s lane choice", scenario)
		}
	}
}

func TestDecisionRequestSingleCallBody(test *testing.T) {
	command := decisionRequestCmd
	for _, name := range []string{"issue", "session", "for", "new-dm", "class", "ask-file", "footer-file", "parent"} {
		flag := command.Flags().Lookup(name)
		if flag == nil {
			test.Fatalf("missing flag %s", name)
		}
		original := flag.Value.String()
		test.Cleanup(func() { _ = command.Flags().Set(name, original) })
	}
	dir := test.TempDir()
	askPath, footerPath := filepath.Join(dir, "ask.txt"), filepath.Join(dir, "footer.txt")
	for path, text := range map[string]string{askPath: structuredAskJSON(test, "Single ask"), footerPath: "Next owner: none"} {
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			test.Fatal(err)
		}
	}
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/decisions" {
			_ = json.NewEncoder(writer).Encode(map[string]any{"id": "11111111-1111-4111-8111-111111111111"})
			return
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			test.Error(err)
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"id": "request-id"})
	}))
	defer server.Close()
	test.Setenv("MULTICA_SERVER_URL", server.URL)
	test.Setenv("MULTICA_TOKEN", "mat_test-token")
	test.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	for name, value := range map[string]string{"issue": "11111111-1111-4111-8111-111111111111", "session": "", "new-dm": "false", "for": "human-id", "class": "reversible", "ask-file": askPath, "footer-file": footerPath, "parent": "comment-id"} {
		_ = command.Flags().Set(name, value)
	}
	if _, err := captureStdout(test, func() error { return runDecisionRequest(command, nil) }); err != nil {
		test.Fatal(err)
	}
	structured, ok := body["ask"].(map[string]any)
	if !ok || body["for"] != "human-id" || body["new_dm"] != false || body["session_id"] != "" || structured["decision"] != "Single ask" || body["footer"] != "Next owner: none" || body["parent_id"] != "comment-id" {
		test.Fatalf("wrong single-call body: %+v", body)
	}
}
