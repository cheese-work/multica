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

func chatContinueTestCommand(summaryFile string) *cobra.Command {
	command := &cobra.Command{Use: "continue"}
	command.Flags().String("summary-file", summaryFile, "")
	command.Flags().String("output", "json", "")
	return command
}

func TestChatContinuePostsSummaryAndReturnsURL(t *testing.T) {
	clearAmbientDaemonTaskEnv(t)
	summaryFile := filepath.Join(t.TempDir(), "summary.md")
	if err := os.WriteFile(summaryFile, []byte("Goal: continue the DM.\nDecision: preserve evidence."), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/chat/continue" {
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil || !strings.Contains(body["summary"], "Decision: preserve evidence.") {
			t.Errorf("summary request: %v %v", body, err)
		}
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusCreated)
		_, _ = response.Write([]byte(`{"url":"/workspace/chat?session=fresh","session":{"id":"fresh"}}`))
	}))
	t.Cleanup(server.Close)
	setCLITestServerEnv(t, server.URL)
	command := chatContinueTestCommand(summaryFile)
	var output bytes.Buffer
	command.SetOut(&output)
	if err := runChatContinue(command, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "/workspace/chat?session=fresh") {
		t.Fatalf("missing new DM URL: %s", output.String())
	}
}

func TestChatContinueRejectsInvalidSummaryWithoutHTTP(t *testing.T) {
	for _, content := range []string{"", " \n", string([]byte{0xff}), strings.Repeat("x", 65537)} {
		summaryFile := filepath.Join(t.TempDir(), "summary.md")
		if err := os.WriteFile(summaryFile, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if err := runChatContinue(chatContinueTestCommand(summaryFile), nil); err == nil {
			t.Fatal("invalid summary was accepted")
		}
	}
	if err := runChatContinue(chatContinueTestCommand(""), nil); err == nil {
		t.Fatal("missing file was accepted")
	}
}

func chatForkTestCommand(summaryFile, key string) *cobra.Command {
	command := chatContinueTestCommand(summaryFile)
	command.Flags().Bool("fork", true, "")
	command.Flags().String("idempotency-key", key, "")
	return command
}

func TestChatContinueForkFlagPostsToForkAPI(t *testing.T) {
	clearAmbientDaemonTaskEnv(t)
	flag := chatContinueCmd.Flags().Lookup("fork")
	if flag == nil || flag.DefValue != "false" {
		t.Fatal("chat continue must expose --fork, disabled by default")
	}
	if !strings.Contains(chatContinueCmd.Long, "archives") || !strings.Contains(chatContinueCmd.Long, "--fork") {
		t.Fatalf("help does not contrast archive and --fork: %q", chatContinueCmd.Long)
	}
	status := http.StatusCreated
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/chat/fork" {
			t.Errorf("unexpected fork request: %s %s", request.Method, request.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body["idempotency_key"] != "retryable-operation" || len(body) != 1 {
			t.Errorf("fork request: %v %v", body, err)
		}
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(status)
		_, _ = response.Write([]byte(`{"url":"/workspace/chat?session=fork","session":{"id":"fork"}}`))
	}))
	t.Cleanup(server.Close)
	setCLITestServerEnv(t, server.URL)
	command := chatForkTestCommand("", "retryable-operation")
	var output bytes.Buffer
	command.SetOut(&output)
	if err := runChatContinue(command, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "/workspace/chat?session=fork") {
		t.Fatalf("missing fork URL: %s", output.String())
	}
	status = http.StatusConflict
	if err := runChatContinue(chatForkTestCommand("", "retryable-operation"), nil); err == nil || !strings.HasPrefix(err.Error(), "fork chat:") {
		t.Fatalf("fork error = %v, want a fork chat: prefix", err)
	}
}

func TestChatContinueForkRejectsSummaryWithoutHTTP(t *testing.T) {
	summaryFile := filepath.Join(t.TempDir(), "summary.md")
	if err := os.WriteFile(summaryFile, []byte("Goal"), 0600); err != nil {
		t.Fatal(err)
	}
	err := runChatContinue(chatForkTestCommand(summaryFile, ""), nil)
	if err == nil || !strings.HasPrefix(err.Error(), "fork chat:") || !strings.Contains(err.Error(), "--summary-file") {
		t.Fatalf("fork with a summary = %v", err)
	}
}

func TestChatContinueTablePrefersWebURL(t *testing.T) {
	clearAmbientDaemonTaskEnv(t)
	summaryFile := filepath.Join(t.TempDir(), "summary.md")
	if err := os.WriteFile(summaryFile, []byte("Goal: continue the DM."), 0600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ body, want string }{
		"web_url present": {`{"url":"/ws/chat?session=x","web_url":"https://app.test/ws/chat?session=x","session":{"id":"x"}}`, "https://app.test/ws/chat?session=x"},
		"web_url absent":  {`{"url":"/ws/chat?session=x","session":{"id":"x"}}`, "/ws/chat?session=x"},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "application/json")
				_, _ = response.Write([]byte(tc.body))
			}))
			t.Cleanup(server.Close)
			setCLITestServerEnv(t, server.URL)
			command := chatContinueTestCommand(summaryFile)
			_ = command.Flags().Set("output", "table")
			var output bytes.Buffer
			command.SetOut(&output)
			if err := runChatContinue(command, nil); err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(output.String()); got != tc.want {
				t.Fatalf("table output = %q, want %q", got, tc.want)
			}
		})
	}
}
