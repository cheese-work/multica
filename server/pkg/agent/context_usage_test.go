package agent

import (
	"context"
	"log/slog"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// resultContext reads Result.ContextTokens / ContextWindow by name so these
// tests build against a tree that has no such fields yet and fail on the
// missing behavior instead of on compilation (Test-First Gate).
func resultContext(t *testing.T, r Result) (tokens, window int64) {
	t.Helper()
	v := reflect.ValueOf(r)
	for name, dst := range map[string]*int64{"ContextTokens": &tokens, "ContextWindow": &window} {
		f := v.FieldByName(name)
		if !f.IsValid() {
			t.Fatalf("agent.Result has no %s field", name)
		}
		*dst = f.Int()
	}
	return tokens, window
}

// The context size is the prompt of the LAST main-loop model call, not a sum
// over the run, and a sub-model call (parent_tool_use_id set) never counts.
func TestClaudeExecuteReportsLastMainCallContext(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}

	assistant := func(id, model, parent string, in, read, write int) string {
		p := ""
		if parent != "" {
			p = `"parent_tool_use_id":"` + parent + `",`
		}
		return `printf '%s\n' '{"type":"assistant",` + p + `"message":{"id":"` + id + `","role":"assistant","model":"` + model +
			`","content":[{"type":"text","text":"x"}],"usage":{"input_tokens":` + strconv.Itoa(in) + `,"output_tokens":5,"cache_read_input_tokens":` +
			strconv.Itoa(read) + `,"cache_creation_input_tokens":` + strconv.Itoa(write) + `}}}'` + "\n"
	}
	script := "#!/bin/sh\n" +
		"IFS= read -r _\n" +
		`printf '%s\n' '{"type":"system","session_id":"sess-ctx"}'` + "\n" +
		assistant("m1", "claude-opus-4-7", "", 1000, 5000, 200) +
		assistant("s1", "claude-haiku-4-5", "toolu_1", 900000, 0, 0) +
		assistant("m2", "claude-opus-4-7", "", 10, 20000, 300) +
		assistant("s2", "claude-haiku-4-5", "toolu_2", 800000, 0, 0) +
		`printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"session_id":"sess-ctx","result":"done","modelUsage":{` +
		`"claude-opus-4-7":{"inputTokens":1010,"outputTokens":10,"cacheReadInputTokens":25000,"cacheCreationInputTokens":500,"contextWindow":200000},` +
		`"claude-haiku-4-5":{"inputTokens":1700000,"outputTokens":10,"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"contextWindow":100000}}}'` + "\n"

	fakePath := filepath.Join(t.TempDir(), "claude")
	writeTestExecutable(t, fakePath, []byte(script))
	backend, err := New("claude", Config{ExecutablePath: fakePath, Env: map[string]string{"IS_SANDBOX": "1"}, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new claude backend: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "prompt-ignored", ExecOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	go func() {
		for range session.Messages {
		}
	}()

	select {
	case result := <-session.Result:
		tokens, window := resultContext(t, result)
		if tokens != 20310 || window != 200000 {
			t.Fatalf("context = %d of %d, want 20310 of 200000 (last main call: input+cache read+cache write)", tokens, window)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for result")
	}
}

// Several thread/tokenUsage/updated notifications in one turn: the latest
// `last.inputTokens` is the context (OpenAI input already includes cached
// input), and the window comes from the same notification.
func TestCodexExecuteReportsLatestSnapshotContext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}

	usage := func(total, last, window int) string {
		u := func(n int) string {
			return `{"inputTokens":` + strconv.Itoa(n) + `,"cachedInputTokens":0,"outputTokens":10,"reasoningOutputTokens":0,"totalTokens":` + strconv.Itoa(n+10) + `}`
		}
		return `echo '{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"thr-ctx","turnId":"turn-ctx","tokenUsage":{"total":` +
			u(total) + `,"last":` + u(last) + `,"modelContextWindow":` + strconv.Itoa(window) + `}}}'` + "\n"
	}
	fakePath := writeFakeCodexAppServer(t, ""+
		`read line`+"\n"+
		`echo '{"jsonrpc":"2.0","id":1,"result":{}}'`+"\n"+
		`read line`+"\n"+
		`read line`+"\n"+
		`echo '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thr-ctx"}}}'`+"\n"+
		`read line`+"\n"+
		`echo '{"jsonrpc":"2.0","id":3,"result":{}}'`+"\n"+
		`echo '{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"thr-ctx","turn":{"id":"turn-ctx"}}}'`+"\n"+
		usage(1000, 1000, 258000)+
		usage(6000, 5000, 258000)+
		usage(15000, 9000, 258000)+
		`echo '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thr-ctx","turn":{"id":"turn-ctx","status":"completed"}}}'`+"\n")

	result := executeFakeCodex(t, fakePath, ExecOptions{Model: "gpt-test", Timeout: 10 * time.Second})
	if result.Status != "completed" {
		t.Fatalf("unexpected result: %+v", result)
	}
	tokens, window := resultContext(t, result)
	if tokens != 9000 || window != 258000 {
		t.Fatalf("context = %d of %d, want 9000 of 258000 (latest snapshot, not the first or a sum)", tokens, window)
	}
}
