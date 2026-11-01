package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const humanAskAgentID = "abcdef12-1111-4111-8111-111111111111"

func TestGuardCommentHumanAsk(test *testing.T) {
	for _, testCase := range []struct {
		name    string
		content string
		agentID string
		blocked bool
	}{
		{name: "che2531 shape", content: "**Cheese:** merge PR #601.", agentID: humanAskAgentID, blocked: true},
		{name: "human author is not guarded", content: "**Cheese:** merge PR #601."},
		{name: "escape line", content: "Human action: none\n**Cheese:** merge PR #601.", agentID: humanAskAgentID},
		{name: "plain status", content: "Tests green on the exact head.", agentID: humanAskAgentID},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			err := guardCommentHumanAsk(testCase.content, testCase.agentID)
			if (err != nil) != testCase.blocked {
				test.Fatalf("err=%v, want blocked=%t", err, testCase.blocked)
			}
			if err != nil && !strings.Contains(err.Error(), "line 1: human addressee needs a real member mention link") {
				test.Fatalf("err=%v, want the line-numbered mention problem", err)
			}
		})
	}
}

func TestCommentHumanAskBlockedBeforeRequest(test *testing.T) {
	for _, operation := range []string{"add", "update"} {
		test.Run(operation, func(test *testing.T) {
			test.Chdir(test.TempDir())
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests++
				writer.WriteHeader(http.StatusForbidden)
			}))
			defer server.Close()
			setCLITestServerEnv(test, server.URL)
			test.Setenv("MULTICA_TOKEN", "mat_test-token")
			test.Setenv("MULTICA_AGENT_ID", humanAskAgentID)
			command := newIssueCommentAddTestCmd()
			run := runIssueCommentAdd
			if operation == "update" {
				command = newIssueCommentUpdateTestCmd()
				if err := command.Flags().Set("expected-revision", "1"); err != nil {
					test.Fatal(err)
				}
				run = runIssueCommentUpdate
			}
			if err := command.Flags().Set("content", "**Cheese:** merge PR #601."); err != nil {
				test.Fatal(err)
			}
			err := run(command, []string{"11111111-1111-4111-8111-111111111111"})
			if err == nil || !strings.Contains(err.Error(), "human-ask check") || requests != 0 {
				test.Fatalf("err=%v requests=%d, want human-ask refusal and zero requests", err, requests)
			}
		})
	}
}

func TestWarnCommentHumanAsk(test *testing.T) {
	const ask = "What this is: a.\nWhy you, why now: b.\n[@Cheese](mention://member/f0c4c4cf-b6cd-4939-bfba-a5e14290c1ff) merge it.\n"
	for _, testCase := range []struct {
		name    string
		content string
		agentID string
		want    string
	}{
		{name: "format rules warn", content: ask, agentID: humanAskAgentID, want: "warning: line 1: missing Steps section"},
		{name: "human author is not warned", content: ask},
		{name: "escape line", content: "Human action: none\n" + ask, agentID: humanAskAgentID},
		{name: "plain status", content: "Tests green.", agentID: humanAskAgentID},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			var out bytes.Buffer
			warnCommentHumanAsk(testCase.content, testCase.agentID, &out)
			if testCase.want == "" && out.Len() != 0 || !strings.Contains(out.String(), testCase.want) {
				test.Fatalf("stderr=%q, want %q", out.String(), testCase.want)
			}
		})
	}
}

func TestGuardCommentHumanAskListsWarningsToo(test *testing.T) {
	err := guardCommentHumanAsk("**Cheese:** merge PR #601.", humanAskAgentID)
	if err == nil || !strings.Contains(err.Error(), "warning: line 1: missing Steps section") {
		test.Fatalf("err=%v, want the format warnings next to the refusal", err)
	}
}
