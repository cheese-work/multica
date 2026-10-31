package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestIssueCommentDecisionRecordFlags(test *testing.T) {
	for _, flag := range []string{"decision-record", "message", "source-comment", "context-message", "class", "excerpt"} {
		if issueCommentAddCmd.Flags().Lookup(flag) == nil {
			test.Fatalf("comment add lacks --%s", flag)
		}
	}
}

func TestIssueCommentDecisionRecordBody(test *testing.T) {
	const issueID = "11111111-1111-4111-8111-111111111111"
	const contextID = "44444444-4444-4444-8444-444444444444"
	messageIDs := []string{"22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"}
	for _, asked := range []bool{false, true} {
		test.Run(map[bool]string{false: "unprompted", true: "asked"}[asked], func(test *testing.T) {
			var body map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet {
					_ = json.NewEncoder(writer).Encode(map[string]any{"id": issueID, "identifier": "TST-1"})
					return
				}
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					test.Error(err)
				}
				_ = json.NewEncoder(writer).Encode(map[string]any{"id": issueID, "decision_answer_id": messageIDs[0]})
			}))
			defer server.Close()
			test.Setenv("MULTICA_SERVER_URL", server.URL)
			test.Setenv("MULTICA_TOKEN", "mat_test-token")
			test.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
			command := newIssueCommentAddTestCmd()
			command.Flags().Bool("decision-record", true, "")
			command.Flags().StringArray("message", messageIDs, "")
			command.Flags().String("class", "", "")
			command.Flags().String("context-message", "", "")
			_ = command.Flags().Set("context-message", contextID)
			if asked {
				_ = command.Flags().Set("parent", issueID)
				_ = command.Flags().Set("content", "Agent text")
			} else {
				_ = command.Flags().Set("class", "deploy")
			}
			if _, err := captureStdout(test, func() error { return runIssueCommentAdd(command, []string{issueID}) }); err != nil {
				test.Fatal(err)
			}
			if body["decision_record"] != true || !reflect.DeepEqual(body["message_ids"], []any{messageIDs[0], messageIDs[1]}) {
				test.Fatalf("missing ordered record inputs: %+v", body)
			}
			if body["context_message_id"] != contextID {
				test.Fatalf("missing offered-options message: %+v", body)
			}
			if asked && (body["parent_id"] != issueID || body["content"] != "Agent text") || !asked && body["escalation_class"] != "deploy" {
				test.Fatalf("wrong context/class: %+v", body)
			}
		})
	}
}

func TestIssueCommentDecisionRecordLocalRefusals(test *testing.T) {
	for _, mutation := range []string{"no_messages", "intent", "orphan_messages", "orphan_class", "attachments"} {
		test.Run(mutation, func(test *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests++
				_ = json.NewEncoder(writer).Encode(map[string]any{"id": "11111111-1111-4111-8111-111111111111"})
			}))
			defer server.Close()
			test.Setenv("MULTICA_SERVER_URL", server.URL)
			test.Setenv("MULTICA_TOKEN", "mat_test-token")
			command := newIssueCommentAddTestCmd()
			command.Flags().Bool("decision-record", true, "")
			command.Flags().Bool("decision-intent", false, "")
			messageIDs := []string{"source"}
			if mutation == "no_messages" || mutation == "orphan_class" {
				messageIDs = nil
			}
			command.Flags().StringArray("message", messageIDs, "")
			command.Flags().String("class", "", "")
			switch mutation {
			case "intent":
				_ = command.Flags().Set("decision-intent", "true")
			case "orphan_messages", "orphan_class":
				_ = command.Flags().Set("decision-record", "false")
				_ = command.Flags().Set("class", "deploy")
			case "attachments":
				_ = command.Flags().Set("attachment", "missing.txt")
			}
			if err := runIssueCommentAdd(command, []string{"TST-1"}); err == nil || requests != 0 || !strings.Contains(err.Error(), "--decision-record") {
				test.Fatalf("local refusal err=%v network requests=%d", err, requests)
			}
		})
	}
}

func TestIssueCommentDecisionRecordExcerptBody(test *testing.T) {
	const issueID = "11111111-1111-4111-8111-111111111111"
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet {
			_ = json.NewEncoder(writer).Encode(map[string]any{"id": issueID, "identifier": "TST-1"})
			return
		}
		_ = json.NewDecoder(request.Body).Decode(&body)
		_ = json.NewEncoder(writer).Encode(map[string]any{"id": issueID})
	}))
	defer server.Close()
	test.Setenv("MULTICA_SERVER_URL", server.URL)
	test.Setenv("MULTICA_TOKEN", "mat_test-token")
	test.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	command := newIssueCommentAddTestCmd()
	command.Flags().Bool("decision-record", true, "")
	command.Flags().StringArray("message", []string{"22222222-2222-4222-8222-222222222222"}, "")
	command.Flags().String("class", "deploy", "")
	command.Flags().StringArray("excerpt", []string{"yes", "A"}, "")
	if _, err := captureStdout(test, func() error { return runIssueCommentAdd(command, []string{issueID}) }); err != nil {
		test.Fatal(err)
	}
	if !reflect.DeepEqual(body["excerpts"], []any{"yes", "A"}) {
		test.Fatalf("excerpts not sent in order: %+v", body)
	}

	orphan := newIssueCommentAddTestCmd()
	orphan.Flags().Bool("decision-record", false, "")
	orphan.Flags().StringArray("excerpt", []string{"yes"}, "")
	if err := runIssueCommentAdd(orphan, []string{issueID}); err == nil || !strings.Contains(err.Error(), "--excerpt") {
		test.Fatalf("--excerpt without --decision-record must be refused locally: %v", err)
	}
}
