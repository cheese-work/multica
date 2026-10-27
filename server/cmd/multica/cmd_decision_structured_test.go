package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func structuredAskFixture() map[string]any {
	return map[string]any{
		"context_eli10": "A release is ready. You pick when it goes out.",
		"agent_did":     "Ran the scoped tests.", "decision": "Choose the deployment window.",
		"stakes_if_wrong": "A bad window could cut service.",
		"options": []map[string]any{
			{"label": "Now", "what_happens": "Deploy at once.", "pros": []string{"Fast"}, "cons": []string{"Risky"}, "cost_or_time": "No extra time"},
			{"label": "Tomorrow", "what_happens": "Deploy after the window opens.", "pros": []string{"Safer"}, "cons": []string{"Slower"}, "cost_or_time": "One day"},
		},
		"recommendation":      "Tomorrow",
		"default_if_no_reply": "Do not deploy.",
		"source_issue_id":     "11111111-1111-4111-8111-111111111111",
		"source_comment_id":   "44444444-4444-4444-8444-444444444444",
	}
}

func structuredAskJSON(test *testing.T, decision string) string {
	test.Helper()
	ask := structuredAskFixture()
	ask["decision"] = decision
	content, err := json.Marshal(ask)
	if err != nil {
		test.Fatal(err)
	}
	return string(content)
}

func TestDecisionRequestRejectsMissingStructuredFields(test *testing.T) {
	command := decisionRequestCmd
	for _, name := range []string{"issue", "session", "for", "new-dm", "class", "ask-file"} {
		original := command.Flags().Lookup(name).Value.String()
		test.Cleanup(func() { _ = command.Flags().Set(name, original) })
	}
	for _, field := range []string{"context_eli10", "agent_did", "decision", "stakes_if_wrong", "options", "recommendation", "default_if_no_reply", "source_issue_id", "source_comment_id"} {
		test.Run(field, func(test *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests++
				_ = json.NewEncoder(writer).Encode(map[string]any{"id": "11111111-1111-4111-8111-111111111111"})
			}))
			defer server.Close()
			test.Setenv("MULTICA_SERVER_URL", server.URL)
			test.Setenv("MULTICA_TOKEN", "mat_test-token")
			test.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
			ask := structuredAskFixture()
			delete(ask, field)
			content, err := json.Marshal(ask)
			if err != nil {
				test.Fatal(err)
			}
			path := filepath.Join(test.TempDir(), "ask.json")
			if err := os.WriteFile(path, content, 0600); err != nil {
				test.Fatal(err)
			}
			for name, value := range map[string]string{"issue": "11111111-1111-4111-8111-111111111111", "session": "33333333-3333-4333-8333-333333333333", "for": "", "new-dm": "false", "class": "deploy", "ask-file": path} {
				_ = command.Flags().Set(name, value)
			}
			_, err = captureStdout(test, func() error { return runDecisionRequest(command, nil) })
			if err == nil || requests != 0 {
				test.Fatalf("missing %s accepted or sent to API: error=%v requests=%d", field, err, requests)
			}
		})
	}
}

func TestDecisionWithdrawCommand(test *testing.T) {
	command, _, err := rootCmd.Find([]string{"decision", "withdraw"})
	if err != nil || command.Name() != "withdraw" {
		test.Fatal("decision withdraw is unavailable")
	}
	const requestID = "22222222-2222-4222-8222-222222222222"
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			test.Error(err)
		}
		if request.Method != http.MethodPost || request.URL.Path != "/api/decisions/"+requestID+"/withdraw" || body["reason"] != "Superseded by the newer candidate." {
			test.Errorf("wrong withdrawal: %s %s %+v", request.Method, request.URL, body)
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"id": requestID, "status": "cancelled"})
	}))
	defer server.Close()
	test.Setenv("MULTICA_SERVER_URL", server.URL)
	test.Setenv("MULTICA_TOKEN", "mat_test-token")
	test.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	_ = command.Flags().Set("reason", " ")
	if err := command.RunE(command, []string{requestID}); err == nil || requests != 0 {
		test.Fatal("empty reason reached the API")
	}
	_ = command.Flags().Set("reason", "Superseded by the newer candidate.")
	if _, err := captureStdout(test, func() error { return command.RunE(command, []string{requestID}) }); err != nil || requests != 1 {
		test.Fatalf("withdrawal: error=%v requests=%d", err, requests)
	}
}

func TestPrintAskHintsDedupesAndKeepsStdoutShape(test *testing.T) {
	var stderr bytes.Buffer
	command := &cobra.Command{}
	command.SetErr(&stderr)
	result := map[string]any{"id": "x", "hints": []any{"a", "b"}}
	printAskHints(command, []string{"a"}, result)
	if got := stderr.String(); got != "hint: a\nhint: b\n" {
		test.Fatalf("stderr = %q", got)
	}
	if _, ok := result["hints"]; ok {
		test.Fatal("hints leaked into the JSON result")
	}
}

func TestAskFileHelpNamesBlockingRules(test *testing.T) {
	usage := decisionRequestCmd.Flags().Lookup("ask-file").Usage
	for _, want := range []string{"one question", "numbered list", "bare ID", "agent_did", "options[].label", "options[].cost_or_time"} {
		if !strings.Contains(usage, want) {
			test.Fatalf("--ask-file help lacks %q: %s", want, usage)
		}
	}
}

func TestDecisionRequestAskFileHelpListsHumanSteps(test *testing.T) {
	usage := decisionRequestCmd.Flags().Lookup("ask-file").Usage
	for _, field := range []string{"human_steps[{action, command, values[{name, value, source}], verify_done}]", "probes[]", "after_done"} {
		if !strings.Contains(usage, field) {
			test.Fatalf("--ask-file help is missing %q: %s", field, usage)
		}
	}
}
