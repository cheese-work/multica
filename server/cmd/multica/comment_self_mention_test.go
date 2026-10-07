package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCommentSelfMentionBlockedBeforeRequest(t *testing.T) {
	const authorID = "abcdef12-1111-4111-8111-111111111111"
	for _, operation := range []string{"add", "update"} {
		t.Run(operation, func(t *testing.T) {
			t.Chdir(t.TempDir())
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests++
				writer.WriteHeader(http.StatusForbidden)
			}))
			defer server.Close()
			setCLITestServerEnv(t, server.URL)
			t.Setenv("MULTICA_TOKEN", "mat_test-token")
			t.Setenv("MULTICA_AGENT_ID", authorID)
			command := newIssueCommentAddTestCmd()
			run := runIssueCommentAdd
			if operation == "update" {
				command = newIssueCommentUpdateTestCmd()
				if err := command.Flags().Set("expected-revision", "1"); err != nil {
					t.Fatal(err)
				}
				run = runIssueCommentUpdate
			}
			if err := command.Flags().Set("content", "Reviewer: [self](mention://agent/"+strings.ToUpper(authorID)+")"); err != nil {
				t.Fatal(err)
			}
			err := run(command, []string{"11111111-1111-4111-8111-111111111111"})
			if err == nil || !strings.Contains(err.Error(), "self-mention") || requests != 0 {
				t.Fatalf("err=%v requests=%d, want self-mention lint and zero requests", err, requests)
			}
		})
	}
}

func TestGuardCommentSelfMention(t *testing.T) {
	const authorID = "abcdef12-1111-4111-8111-111111111111"
	const otherID = "abcdef12-1111-4111-8111-222222222222"
	for _, testCase := range []struct {
		name    string
		content string
		agentID string
		blocked bool
	}{
		{name: "direct", content: "[self](mention://agent/" + authorID + ")", agentID: authorID, blocked: true},
		{name: "uppercase", content: "[@self](mention://agent/" + strings.ToUpper(authorID) + ")", agentID: authorID, blocked: true},
		{name: "server normalization", content: "[self](mention://agent/abc\x00def12-1111-4111-8111-111111111111)", agentID: authorID, blocked: true},
		{name: "other recipient", content: "[other](mention://agent/" + otherID + ")", agentID: authorID},
		{name: "human author", content: "[recipient](mention://agent/" + authorID + ")"},
		{name: "different mention type", content: "[issue](mention://issue/" + authorID + ")", agentID: authorID},
		{name: "plain author", content: "Return assignee: test-author", agentID: authorID},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := guardCommentSelfMention(testCase.content, testCase.agentID)
			if (err != nil) != testCase.blocked {
				t.Fatalf("err=%v, want blocked=%t", err, testCase.blocked)
			}
		})
	}
}
