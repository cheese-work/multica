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
