package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestIssueCommentDecisionVetoFlag(test *testing.T) {
	if issueCommentAddCmd.Flags().Lookup("decision-veto") == nil {
		test.Fatal("comment add lacks --decision-veto")
	}
}

func TestIssueCommentDecisionVetoBody(test *testing.T) {
	const issueID = "11111111-1111-4111-8111-111111111111"
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
	command.Flags().Bool("decision-veto", true, "")
	path := filepath.Join(test.TempDir(), "reason.md")
	test.Chdir(filepath.Dir(path))
	if err := os.WriteFile(path, []byte("Wrong source message"), 0600); err != nil {
		test.Fatal(err)
	}
	_ = command.Flags().Set("content-file", path)
	_ = command.Flags().Set("parent", issueID)
	if _, err := captureStdout(test, func() error { return runIssueCommentAdd(command, []string{issueID}) }); err != nil {
		test.Fatal(err)
	}
	if body["decision_veto"] != true || body["parent_id"] != issueID || body["content"] != "Wrong source message" {
		test.Fatalf("missing veto inputs: %+v", body)
	}
}

func TestIssueCommentDecisionVetoLocalRefusals(test *testing.T) {
	for _, mutation := range []string{"no_parent", "inline_reason", "empty_reason", "record", "intent", "messages", "class", "attachments"} {
		test.Run(mutation, func(test *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests++
				_ = json.NewEncoder(writer).Encode(map[string]any{"id": "11111111-1111-4111-8111-111111111111"})
			}))
			defer server.Close()
			test.Setenv("MULTICA_SERVER_URL", server.URL)
			test.Setenv("MULTICA_TOKEN", "mat_test-token")
			test.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
			command := newIssueCommentAddTestCmd()
			command.Flags().Bool("decision-veto", true, "")
			command.Flags().Bool("decision-record", mutation == "record", "")
			command.Flags().Bool("decision-intent", mutation == "intent", "")
			command.Flags().StringArray("message", nil, "")
			command.Flags().String("class", "", "")
			path := filepath.Join(test.TempDir(), "reason.md")
			test.Chdir(filepath.Dir(path))
			reason := "Wrong message"
			if mutation == "empty_reason" {
				reason = " \n\t"
			}
			if err := os.WriteFile(path, []byte(reason), 0600); err != nil {
				test.Fatal(err)
			}
			_ = command.Flags().Set("content-file", path)
			_ = command.Flags().Set("parent", "11111111-1111-4111-8111-111111111111")
			switch mutation {
			case "no_parent":
				_ = command.Flags().Set("parent", "")
			case "inline_reason":
				_ = command.Flags().Set("content-file", "")
				_ = command.Flags().Set("content", reason)
			case "messages":
				_ = command.Flags().Set("message", "11111111-1111-4111-8111-111111111111")
			case "class":
				_ = command.Flags().Set("class", "spend")
			case "attachments":
				_ = command.Flags().Set("attachment", path)
			}
			if _, err := captureStdout(test, func() error { return runIssueCommentAdd(command, []string{"11111111-1111-4111-8111-111111111111"}) }); err == nil || requests != 0 {
				test.Fatalf("veto was not refused locally: err=%v requests=%d", err, requests)
			}
		})
	}
}
