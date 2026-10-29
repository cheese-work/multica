package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runFakeClaudeTurnResult() {
	reader := bufio.NewReader(os.Stdin)
	read := func(want string) {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			fmt.Fprintf(os.Stderr, "read %s: %v\n", want, err)
			os.Exit(21)
		}
		var frame claudeSDKMessage
		if err := json.Unmarshal(line, &frame); err != nil || frame.Type != want {
			fmt.Fprintf(os.Stderr, "expected %s frame, got %s (error=%v)\n", want, line, err)
			os.Exit(22)
		}
	}
	emitInjected := func() {
		data, err := os.ReadFile(os.Getenv("CLAUDE_RESULT_FIXTURE"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(23)
		}
		if _, err := os.Stdout.Write(data); err != nil {
			os.Exit(24)
		}
	}
	mode := os.Getenv("CLAUDE_RESULT_CASE")
	if os.Getenv("CLAUDE_RESULT_SUPPLEMENTS") == "true" {
		read("control_request")
		if mode == "injected_before_prompt" {
			emitInjected()
		}
		fmt.Println(`{"type":"control_response","response":{"subtype":"success","request_id":"multica-supplement-initialize","response":{}}}`)
	} else if mode == "injected_before_prompt" {
		emitInjected()
	}
	read("user")
	fmt.Println(`{"type":"system","subtype":"init","session_id":"owned-session"}`)
	if mode == "injected_after_prompt" {
		emitInjected()
	}
	if strings.HasPrefix(mode, "injected_") {
		_ = json.NewEncoder(os.Stdout).Encode(claudeSupplementHook("UserPromptSubmit", ""))
		read("control_response")
	}
	switch mode {
	case "cancel":
	case "error":
		fmt.Println(`{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"owned-session","result":""}`)
	case "empty":
		fmt.Println(`{"type":"result","subtype":"success","origin":{"kind":"human"},"session_id":"owned-session","result":""}`)
	default:
		fmt.Println(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Multica prompt completed"}]}}`)
		fmt.Println(`{"type":"result","subtype":"success","session_id":"owned-session","result":"Multica prompt completed"}`)
	}
	if _, err := reader.ReadBytes('\n'); err != io.EOF {
		fmt.Fprintf(os.Stderr, "expected stdin EOF after owned turn, got %v\n", err)
		os.Exit(25)
	}
}

func TestClaudeExecuteTurnResult(t *testing.T) {
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := filepath.Abs("testdata/claude-code-2.1.308-injected-result.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, supplements := range []bool{true, false} {
		for _, mode := range []string{"injected_before_prompt", "injected_after_prompt", "normal", "empty", "error", "cancel"} {
			t.Run(fmt.Sprintf("%s/supplements=%t", mode, supplements), func(t *testing.T) {
				t.Parallel()
				backend := &claudeBackend{cfg: Config{
					ExecutablePath: self,
					Logger:         slog.Default(),
					Env: map[string]string{
						"CLAUDE_FAKE_MODE":          "turn_result",
						"CLAUDE_RESULT_CASE":        mode,
						"CLAUDE_RESULT_SUPPLEMENTS": fmt.Sprint(supplements),
						"CLAUDE_RESULT_FIXTURE":     fixture,
						"CLAUDE_CONFIG_DIR":         t.TempDir(),
						"IS_SANDBOX":                "1",
					},
				}}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				session, err := backend.Execute(ctx, "Multica prompt", ExecOptions{
					ResumeSessionID:      "owned-session",
					EnableTaskSupplement: supplements,
				})
				if err != nil {
					t.Fatal(err)
				}
				for message := range session.Messages {
					if mode == "cancel" && message.SessionID == "owned-session" {
						cancel()
					}
				}
				result, ok := <-session.Result
				if !ok {
					t.Fatal("result channel closed without a result")
				}
				status, output, failure := "completed", "Multica prompt completed", ""
				switch mode {
				case "empty":
					output = ""
				case "error":
					status, output, failure = "failed", "", "claude returned an error result without details"
				case "cancel":
					status, output, failure = "aborted", "", "execution cancelled"
				}
				if result.Status != status || result.Output != output || result.Error != failure || result.SessionID != "owned-session" {
					t.Fatalf("expected status=%s output=%q error=%q session=owned-session, got %+v", status, output, failure, result)
				}
				if session.SupplementReady != nil && session.SupplementReady() {
					t.Fatal("completed turn still accepts supplements")
				}
			})
		}
	}
}
