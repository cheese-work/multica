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
	read := func(want string) string {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			fmt.Fprintf(os.Stderr, "read %s: %v\n", want, err)
			os.Exit(21)
		}
		var frame struct {
			Type string `json:"type"`
			UUID string `json:"uuid"`
		}
		if err := json.Unmarshal(line, &frame); err != nil || frame.Type != want {
			fmt.Fprintf(os.Stderr, "expected %s frame, got %s (error=%v)\n", want, line, err)
			os.Exit(22)
		}
		return frame.UUID
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
	promptUUID := read("user")
	fmt.Println(`{"type":"system","subtype":"init","session_id":"owned-session"}`)
	if mode == "injected_after_prompt" {
		emitInjected()
	}
	if strings.HasPrefix(mode, "injected_") {
		_ = json.NewEncoder(os.Stdout).Encode(claudeSupplementHook("UserPromptSubmit", ""))
		read("control_response")
	}
	result := map[string]any{
		"type": "result", "subtype": "success", "session_id": "owned-session",
		"is_error": false, "result_index": 1, "queued_turn_count": 0,
		"user_message_uuid": promptUUID,
	}
	if mode == "injected_mismatched_uuid" {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"type": "result", "subtype": "success", "session_id": "owned-session",
			"result_index": 0, "user_message_uuid": "b60ccafe-dc53-4479-a651-11063fa3ed32",
		})
		_ = json.NewEncoder(os.Stdout).Encode(claudeSupplementHook("UserPromptSubmit", ""))
		read("control_response")
	}
	if mode == "folded_prompt" {
		result["user_message_uuid"] = "b60ccafe-dc53-4479-a651-11063fa3ed32"
		result["user_message_uuids"] = []string{promptUUID, "b60ccafe-dc53-4479-a651-11063fa3ed32"}
	}
	if strings.HasPrefix(mode, "legacy_") {
		delete(result, "result_index")
		delete(result, "queued_turn_count")
		delete(result, "user_message_uuid")
	}
	switch mode {
	case "cancel":
	case "error", "legacy_error":
		result["subtype"], result["is_error"], result["result"] = "error_during_execution", true, ""
	case "empty", "legacy_empty":
		result["result"] = ""
	default:
		fmt.Println(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Multica prompt completed"}]}}`)
		result["result"] = "Multica prompt completed"
	}
	if mode != "cancel" {
		_ = json.NewEncoder(os.Stdout).Encode(result)
	}
	if _, err := reader.ReadBytes('\n'); err != io.EOF {
		fmt.Fprintf(os.Stderr, "expected stdin EOF after owned turn, got %v\n", err)
		os.Exit(25)
	}
}

func TestClaudeExecuteTurnResult(test *testing.T) {
	test.Parallel()
	self, err := os.Executable()
	if err != nil {
		test.Fatal(err)
	}
	fixture, err := filepath.Abs("testdata/claude-code-2.1.308-injected-result.jsonl")
	if err != nil {
		test.Fatal(err)
	}
	for _, supplements := range []bool{true, false} {
		for _, mode := range []string{"injected_before_prompt", "injected_after_prompt", "injected_mismatched_uuid", "folded_prompt", "normal", "empty", "error", "cancel", "legacy_normal", "legacy_empty", "legacy_error"} {
			test.Run(fmt.Sprintf("%s/supplements=%t", mode, supplements), func(subtest *testing.T) {
				subtest.Parallel()
				backend := &claudeBackend{cfg: Config{
					ExecutablePath: self,
					Logger:         slog.Default(),
					Env: map[string]string{
						"CLAUDE_FAKE_MODE":          "turn_result",
						"CLAUDE_RESULT_CASE":        mode,
						"CLAUDE_RESULT_SUPPLEMENTS": fmt.Sprint(supplements),
						"CLAUDE_RESULT_FIXTURE":     fixture,
						"CLAUDE_CONFIG_DIR":         subtest.TempDir(),
						"IS_SANDBOX":                "1",
					},
				}}
				ctx, cancel := context.WithTimeout(subtest.Context(), 10*time.Second)
				defer cancel()
				session, err := backend.Execute(ctx, "Multica prompt", ExecOptions{
					ResumeSessionID:      "owned-session",
					EnableTaskSupplement: supplements,
				})
				if err != nil {
					subtest.Fatal(err)
				}
				for message := range session.Messages {
					if mode == "cancel" && message.SessionID == "owned-session" {
						cancel()
					}
				}
				result, ok := <-session.Result
				if !ok {
					subtest.Fatal("result channel closed without a result")
				}
				status, output, failure := "completed", "Multica prompt completed", ""
				switch mode {
				case "empty", "legacy_empty":
					output = ""
				case "error", "legacy_error":
					status, output, failure = "failed", "", "claude returned an error result without details"
				case "cancel":
					status, output, failure = "aborted", "", "execution cancelled"
				}
				if result.Status != status || result.Output != output || result.Error != failure || result.SessionID != "owned-session" {
					subtest.Fatalf("expected status=%s output=%q error=%q session=owned-session, got %+v", status, output, failure, result)
				}
				if session.SupplementReady != nil && session.SupplementReady() {
					subtest.Fatal("completed turn still accepts supplements")
				}
			})
		}
	}
}
