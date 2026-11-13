package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestDecisionCommandsRoundTrip(test *testing.T) {
	const issueID = "11111111-1111-4111-8111-111111111111"
	const requestID = "22222222-2222-4222-8222-222222222222"
	const sessionID = "33333333-3333-4333-8333-333333333333"
	ask := "  Preserve this ask.\nSecond line.  "
	askPath := filepath.Join(test.TempDir(), "ask.txt")
	if err := os.WriteFile(askPath, []byte(structuredAskJSON(test, ask)), 0600); err != nil {
		test.Fatal(err)
	}
	var posted bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/issues/" + issueID, "/api/issues/CHE-1570":
			_ = json.NewEncoder(writer).Encode(map[string]any{"id": issueID, "identifier": "CHE-1570"})
		case "/api/decisions":
			if request.Method == http.MethodPost {
				var body map[string]any
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					test.Error(err)
				}
				structured, ok := body["ask"].(map[string]any)
				if !ok || body["issue_id"] != issueID || body["session_id"] != sessionID || structured["decision"] != ask || body["escalation_class"] != "spend" {
					test.Errorf("wrong request body: %+v", body)
				}
				posted = true
				writer.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(writer).Encode(map[string]any{"id": requestID, "ask": ask})
			} else {
				if !posted || request.URL.Query().Get("issue") != issueID {
					test.Errorf("wrong list request: %s", request.URL)
				}
				_ = json.NewEncoder(writer).Encode([]map[string]any{{"id": requestID, "ask": ask}})
			}
		case "/api/decisions/" + requestID:
			if request.Method != http.MethodGet || !posted {
				test.Errorf("wrong get request: %s %s", request.Method, request.URL)
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"id": requestID, "ask": ask})
		default:
			test.Errorf("unexpected request: %s %s", request.Method, request.URL)
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	test.Setenv("MULTICA_SERVER_URL", server.URL)
	test.Setenv("MULTICA_TOKEN", "mat_test-token")
	test.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	for _, operation := range []string{"request", "get", "list"} {
		command, _, err := rootCmd.Find([]string{"decision", operation})
		if err != nil || command.Name() != operation {
			test.Fatalf("decision %s is unavailable: %v", operation, err)
		}
		_ = command.Flags().Set("output", "json")
		args := []string{}
		switch operation {
		case "request":
			_ = command.Flags().Set("issue", "CHE-1570")
			_ = command.Flags().Set("session", sessionID)
			_ = command.Flags().Set("class", "spend")
			_ = command.Flags().Set("ask-file", askPath)
		case "get":
			args = []string{requestID}
		case "list":
			_ = command.Flags().Set("issue", "CHE-1570")
		}
		output, err := captureStdout(test, func() error { return command.RunE(command, args) })
		if err != nil || !json.Valid([]byte(output)) {
			test.Fatalf("decision %s: output=%q error=%v", operation, output, err)
		}
	}
}

func TestDecisionRequestCommandRequiresAskFile(test *testing.T) {
	command, _, err := rootCmd.Find([]string{"decision", "request"})
	if err != nil || command.Name() != "request" {
		test.Fatal("decision request is unavailable")
	}
	_ = command.Flags().Set("ask-file", "")
	if err := command.RunE(command, nil); err == nil {
		test.Fatal("request without --ask-file was accepted")
	}
}

func TestDecisionRequestCommandRejectsInvalidUTF8(test *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		test.Error("invalid UTF-8 reached the API")
		http.Error(writer, "unexpected request", http.StatusBadRequest)
	}))
	defer server.Close()
	test.Setenv("MULTICA_SERVER_URL", server.URL)
	test.Setenv("MULTICA_TOKEN", "mat_test-token")
	test.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	path := filepath.Join(test.TempDir(), "ask.txt")
	if err := os.WriteFile(path, []byte{0xff}, 0600); err != nil {
		test.Fatal(err)
	}
	command, _, err := rootCmd.Find([]string{"decision", "request"})
	if err != nil {
		test.Fatal(err)
	}
	_ = command.Flags().Set("issue", "11111111-1111-4111-8111-111111111111")
	_ = command.Flags().Set("session", "33333333-3333-4333-8333-333333333333")
	_ = command.Flags().Set("class", "spend")
	_ = command.Flags().Set("ask-file", path)
	if err := command.RunE(command, nil); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		test.Fatalf("invalid UTF-8 was not rejected before serialization: %v", err)
	}
}

func decisionRequestFlags(test *testing.T, askJSON string, flags map[string]string) *cobra.Command {
	test.Helper()
	path := filepath.Join(test.TempDir(), "ask.json")
	if err := os.WriteFile(path, []byte(askJSON), 0600); err != nil {
		test.Fatal(err)
	}
	command, _, err := rootCmd.Find([]string{"decision", "request"})
	if err != nil {
		test.Fatal(err)
	}
	defaults := map[string]string{"issue": "11111111-1111-4111-8111-111111111111", "session": "33333333-3333-4333-8333-333333333333", "class": "reversible", "ask-file": path, "kind": "decision", "expires-in": "", "action-file": ""}
	for name, value := range flags {
		defaults[name] = value
	}
	for name, value := range defaults {
		if err := command.Flags().Set(name, value); err != nil {
			test.Fatal(err)
		}
	}
	test.Cleanup(func() {
		for _, name := range []string{"kind", "expires-in", "action-file"} {
			_ = command.Flags().Set(name, map[string]string{"kind": "decision"}[name])
		}
	})
	return command
}

const cliActionAsk = `{"context_eli10":"A property is missing. Only the workspace owner can create it.","agent_did":"Checked that the property is missing.","why_human":"Owner-only: property create needs the workspace owner.","steps":["Run the create command."],"done_when":"The property is listed.","default_if_no_reply":"Continue without it.","source_issue_id":"11111111-1111-4111-8111-111111111111","source_comment_id":"22222222-2222-4222-8222-222222222222"}`

func TestDecisionRequestCommandSendsActionKind(test *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet {
			_ = json.NewEncoder(writer).Encode(map[string]any{"id": "11111111-1111-4111-8111-111111111111", "identifier": "CHE-1"})
			return
		}
		_ = json.NewDecoder(request.Body).Decode(&body)
		_ = json.NewEncoder(writer).Encode(map[string]any{"id": "44444444-4444-4444-8444-444444444444", "status": "open", "escalation_class": "reversible", "ask": "x"})
	}))
	defer server.Close()
	test.Setenv("MULTICA_SERVER_URL", server.URL)
	test.Setenv("MULTICA_TOKEN", "mat_test-token")
	test.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	command := decisionRequestFlags(test, cliActionAsk, map[string]string{"kind": "action", "expires-in": "48h"})
	if err := command.RunE(command, nil); err != nil {
		test.Fatal(err)
	}
	ask, _ := body["ask"].(map[string]any)
	if body["kind"] != "action" || body["expires_in"] != "48h" || ask["steps"] == nil || ask["options"] != nil || ask["decision"] != nil {
		test.Fatalf("wrong request body: %+v", body)
	}
}

func TestDecisionRequestCommandActionRefusals(test *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		test.Errorf("refused request reached the API: %s %s", request.Method, request.URL.Path)
		http.Error(writer, "unexpected request", http.StatusBadRequest)
	}))
	defer server.Close()
	test.Setenv("MULTICA_SERVER_URL", server.URL)
	test.Setenv("MULTICA_TOKEN", "mat_test-token")
	test.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	actionFile := filepath.Join(test.TempDir(), "action.json")
	if err := os.WriteFile(actionFile, []byte(`{"type":"noop","version":1}`), 0600); err != nil {
		test.Fatal(err)
	}
	cases := []struct {
		name  string
		ask   string
		flags map[string]string
		want  string
	}{
		{"action-file with action", cliActionAsk, map[string]string{"kind": "action", "action-file": actionFile}, "--action-file"},
		{"expires-in zero", cliActionAsk, map[string]string{"kind": "action", "expires-in": "0"}, "--expires-in"},
		{"expires-in malformed", cliActionAsk, map[string]string{"kind": "action", "expires-in": "soon"}, "--expires-in"},
		{"expires-in 169h", cliActionAsk, map[string]string{"kind": "action", "expires-in": "169h"}, "--expires-in"},
		{"expires-in on a decision", cliActionAsk, map[string]string{"kind": "decision", "expires-in": "24h"}, "--expires-in"},
		{"unknown kind", cliActionAsk, map[string]string{"kind": "bogus"}, "--kind"},
		{"decision ask with kind action", `{"decision":"Ship?"}`, map[string]string{"kind": "action"}, "ask.decision"},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			command := decisionRequestFlags(test, tc.ask, tc.flags)
			if err := command.RunE(command, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
				test.Fatalf("error = %v, want mention of %q", err, tc.want)
			}
		})
	}
}
