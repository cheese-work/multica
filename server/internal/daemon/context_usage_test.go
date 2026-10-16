package daemon

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"
)

// setContext fills TaskResult.ContextTokens / ContextWindow by name so these
// tests build against a tree that has no such fields yet and fail on the
// missing behavior instead of on compilation (Test-First Gate).
func setContext(t *testing.T, r *TaskResult, tokens, window int64) {
	t.Helper()
	v := reflect.ValueOf(r).Elem()
	for name, n := range map[string]int64{"ContextTokens": tokens, "ContextWindow": window} {
		f := v.FieldByName(name)
		if !f.IsValid() {
			t.Fatalf("TaskResult has no %s field", name)
		}
		f.SetInt(n)
	}
}

func getContext(t *testing.T, r TaskResult) (tokens, window int64) {
	t.Helper()
	v := reflect.ValueOf(r)
	for name, dst := range map[string]*int64{"ContextTokens": &tokens, "ContextWindow": &window} {
		f := v.FieldByName(name)
		if !f.IsValid() {
			t.Fatalf("TaskResult has no %s field", name)
		}
		*dst = f.Int()
	}
	return tokens, window
}

func TestReportTaskResult_SendsContextOnComplete(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name           string
		tokens, window int64
		want           map[string]any
	}{
		{"known", 164000, 200000, map[string]any{"tokens": float64(164000), "window": float64(200000)}},
		{"unknown_window", 164000, 0, nil},
		{"unknown_tokens", 0, 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := &reportTaskResultRecorder{}
			srv := httptest.NewServer(rec.handler(t))
			t.Cleanup(srv.Close)

			result := TaskResult{Status: "completed", Comment: "done", SessionID: "ses-1", WorkDir: "/tmp/foo"}
			setContext(t, &result, tc.tokens, tc.window)
			d := &Daemon{client: NewClient(srv.URL), logger: slog.Default()}
			d.reportTaskResult(context.Background(), "task-1", result, slog.Default())

			rec.mu.Lock()
			defer rec.mu.Unlock()
			if rec.path != "/api/daemon/tasks/task-1/complete" {
				t.Fatalf("expected /complete endpoint, got %s", rec.path)
			}
			if !reflect.DeepEqual(rec.payload["context"], tc.want) {
				t.Fatalf("context = %#v, want %#v", rec.payload["context"], tc.want)
			}
		})
	}
}

// runTask carries the backend's last-call context size and window onto the
// TaskResult that the terminal report is built from.
func TestRunTaskCarriesContextFromClaude(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script agent fixture is POSIX-only")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	fake := filepath.Join(t.TempDir(), "claude-fixture")
	writeTestExecutable(t, fake, []byte("#!/bin/sh\n"+
		"case \"$*\" in *--version*) echo '2.1.299'; exit 0;; esac\n"+
		"IFS= read -r _\n"+
		`printf '%s\n' '{"type":"system","session_id":"sess-ctx"}'`+"\n"+
		`printf '%s\n' '{"type":"assistant","message":{"id":"m1","role":"assistant","model":"claude-opus-4-7","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":20000,"cache_creation_input_tokens":300}}}'`+"\n"+
		`printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"session_id":"sess-ctx","result":"done","modelUsage":{"claude-opus-4-7":{"inputTokens":10,"outputTokens":5,"cacheReadInputTokens":20000,"cacheCreationInputTokens":300,"contextWindow":200000}}}'`+"\n"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	d := &Daemon{
		client: NewClient(srv.URL), logger: logger,
		workspaces: make(map[string]*workspaceState), activeEnvRoots: make(map[string]int),
		runtimeIndex: map[string]Runtime{"test-rt": {ID: "test-rt", Provider: "claude"}},
		cfg: Config{
			WorkspacesRoot: t.TempDir(), AgentTimeout: 5 * time.Second, ServerBaseURL: srv.URL,
			Agents: map[string]AgentEntry{"claude": {Path: fake}},
		},
	}
	task := Task{
		ID: "test-task", WorkspaceID: "test-ws", RuntimeID: "test-rt", IssueID: "test-issue",
		AgentID: "test-agent", AuthToken: "mat_test_ctx",
		Agent: &AgentData{ID: "test-agent", Name: "test", Model: "claude-opus-4-7"},
	}
	result, err := d.runTask(context.Background(), task, "claude", 0, logger)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" {
		t.Fatalf("unexpected result: %+v", result)
	}
	tokens, window := getContext(t, result)
	if tokens != 20310 || window != 200000 {
		t.Fatalf("context = %d of %d, want 20310 of 200000", tokens, window)
	}
}
