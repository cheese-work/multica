package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/spf13/cobra"
)

const invalidMentionsIssueID = "11111111-1111-4111-8111-111111111111"

// invalidMentionsServer answers the issue lookup, then rejects every comment
// write with the 422 invalid_agent_mentions body and counts the writes.
func invalidMentionsServer(test *testing.T, body string, writes *atomic.Int32) *httptest.Server {
	test.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodGet {
			_ = json.NewEncoder(writer).Encode(map[string]any{"id": invalidMentionsIssueID, "identifier": "CHE-1"})
			return
		}
		writes.Add(1)
		writer.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = writer.Write([]byte(body))
	}))
	test.Cleanup(server.Close)
	setCLITestServerEnv(test, server.URL)
	test.Setenv("MULTICA_TOKEN", "mat_test-token")
	return server
}

// A comment the server refused for a non-invocable agent mention must fail
// loudly: non-zero exit, each rejected range with the text the caller sent,
// "nothing was saved", and exactly one request (a 422 is never retried).
func TestIssueCommentWriteReportsInvalidAgentMentions(test *testing.T) {
	const (
		first   = "[@Ghost](mention://agent/not-a-real-agent)"
		second  = "[@Gone](mention://agent/99999999-9999-4999-8999-999999999999)"
		content = "Next owner: " + first + " and " + second + "."
	)
	firstStart := strings.Index(content, first)
	secondStart := strings.Index(content, second)
	body, _ := json.Marshal(map[string]any{
		"error": "comment mentions agents that cannot be invoked",
		"code":  "invalid_agent_mentions",
		"invalid_mentions": []map[string]int{
			{"start": firstStart, "end": firstStart + len(first)},
			{"start": secondStart, "end": secondStart + len(second)},
		},
	})
	for _, tc := range []struct {
		name string
		run  func(*cobra.Command) error
		cmd  func() *cobra.Command
		set  map[string]string
	}{
		{"add", func(c *cobra.Command) error { return runIssueCommentAdd(c, []string{invalidMentionsIssueID}) },
			newIssueCommentAddTestCmd, nil},
		{"update", func(c *cobra.Command) error {
			return runIssueCommentUpdate(c, []string{"44444444-4444-4444-8444-444444444444"})
		},
			newIssueCommentUpdateTestCmd, map[string]string{"expected-revision": "1"}},
	} {
		for _, output := range []string{"table", "json"} {
			test.Run(tc.name+"/"+output, func(test *testing.T) {
				test.Chdir(test.TempDir())
				var writes atomic.Int32
				invalidMentionsServer(test, string(body), &writes)
				command := tc.cmd()
				flags := map[string]string{"content": content, "output": output}
				for flag, value := range tc.set {
					flags[flag] = value
				}
				for flag, value := range flags {
					if err := command.Flags().Set(flag, value); err != nil {
						test.Fatal(err)
					}
				}
				var runErr error
				stdout, _ := captureStdout(test, func() error {
					runErr = tc.run(command)
					return nil
				})
				if runErr == nil {
					test.Fatal("want a non-nil error so the exit code is non-zero")
				}
				message := cli.FormatError(runErr, false)
				for _, want := range []string{
					"invalid_agent_mentions", "the comment was not saved", "attachments already uploaded stay on the issue",
					"bytes " + strconv.Itoa(firstStart) + "-" + strconv.Itoa(firstStart+len(first)), first,
					"bytes " + strconv.Itoa(secondStart) + "-" + strconv.Itoa(secondStart+len(second)), second,
				} {
					if !strings.Contains(message, want) {
						test.Fatalf("message missing %q:\n%s", want, message)
					}
				}
				if got := writes.Load(); got != 1 {
					test.Fatalf("comment write requests = %d, want 1 (no retry on 422)", got)
				}
				if output == "json" {
					if strings.TrimSpace(stdout) != string(body) {
						test.Fatalf("json mode must pass the server body through unchanged:\n%s", stdout)
					}
				} else if stdout != "" {
					test.Fatalf("table mode wrote to stdout: %q", stdout)
				}
			})
		}
	}
}

// Offsets the CLI cannot trust (out of range, reversed, or inside a UTF-8
// sequence) print the range only; they never panic or slice the wrong text.
func TestInvalidAgentMentionsMessageBoundsChecksRanges(test *testing.T) {
	const sent = "héllo [@A](mention://agent/x)"
	for name, ranges := range map[string]string{
		"past end":      `[{"start":2,"end":999}]`,
		"reversed":      `[{"start":9,"end":3}]`,
		"negative":      `[{"start":-1,"end":4}]`,
		"inside a rune": `[{"start":2,"end":5}]`,
	} {
		test.Run(name, func(test *testing.T) {
			httpErr := &cli.HTTPError{StatusCode: 422, Body: `{"code":"invalid_agent_mentions","invalid_mentions":` + ranges + `}`}
			message, ok := invalidAgentMentionsMessage(httpErr, sent)
			if !ok || !strings.Contains(message, "bytes ") || strings.Contains(message, "hé") {
				test.Fatalf("ok=%v message=%q", ok, message)
			}
		})
	}
	test.Run("other 422 is not ours", func(test *testing.T) {
		httpErr := &cli.HTTPError{StatusCode: 422, Body: `{"code":"something_else"}`}
		if _, ok := invalidAgentMentionsMessage(httpErr, sent); ok {
			test.Fatal("matched a different code")
		}
	})
}
