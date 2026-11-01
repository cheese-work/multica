package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// appliedRun is one runTask through a fake claude CLI, with the server's
// applied-settings answer chosen by the test.
type appliedRun struct {
	posts  []map[string]any
	argv   string // what the CLI was launched with; empty when it never ran
	result TaskResult
}

func runAppliedTask(t *testing.T, status int, supported bool, entryModel string, claudeArgs []string, agentData AgentData) appliedRun {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script agent fixture is POSIX-only")
	}
	argvFile := filepath.Join(t.TempDir(), "argv")
	fake := filepath.Join(t.TempDir(), "claude-fixture")
	writeTestExecutable(t, fake, []byte("#!/bin/sh\n"+
		"case \"$*\" in *--version*) echo '2.1.299'; exit 0;; esac\n"+
		"printf '%s ' \"$@\" > "+argvFile+"\n"+
		"IFS= read -r _\n"+
		`printf '%s\n' '{"type":"system","session_id":"sess-applied"}'`+"\n"+
		`printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"session_id":"sess-applied","result":"done"}'`+"\n"))

	var mu sync.Mutex
	var run appliedRun
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/applied-settings") {
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("applied-settings body: %v", err)
			}
			mu.Lock()
			run.posts = append(run.posts, body)
			mu.Unlock()
			w.WriteHeader(status)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := &Daemon{
		client: NewClient(srv.URL), logger: logger,
		workspaces: make(map[string]*workspaceState), activeEnvRoots: make(map[string]int),
		runtimeIndex: map[string]Runtime{"test-rt": {ID: "test-rt", Provider: "claude"}},
		cfg: Config{
			WorkspacesRoot: t.TempDir(), AgentTimeout: 5 * time.Second, ServerBaseURL: srv.URL,
			ClaudeArgs: claudeArgs,
			Agents:     map[string]AgentEntry{"claude": {Path: fake, Model: entryModel}},
		},
	}
	agentData.ID, agentData.Name = "test-agent", "test"
	task := Task{
		ID: "task-1", WorkspaceID: "test-ws", RuntimeID: "test-rt", IssueID: "test-issue",
		AgentID: "test-agent", AuthToken: "mat_test_applied", Agent: &agentData,
		DispatchedAt: "2026-09-17T09:00:00.123456Z", StartClaimSupported: true, AppliedSettingsSupported: supported,
	}
	result, err := d.runTask(context.Background(), task, "claude", 0, logger)
	if err != nil {
		t.Fatal(err)
	}
	run.result = result
	if raw, err := os.ReadFile(argvFile); err == nil {
		run.argv = string(raw)
	}
	mu.Lock()
	defer mu.Unlock()
	return run
}

// R3: with no model on the claim, the env-configured model that the launch uses
// is the model reported.
func TestReportAppliedSettings_R3_ReportsResolvedSelection(t *testing.T) {
	run := runAppliedTask(t, http.StatusOK, true, "env-model", nil, AgentData{})

	if len(run.posts) != 1 {
		t.Fatalf("posts = %v, want 1", run.posts)
	}
	want := map[string]any{
		"runtime_id": "test-rt", "dispatched_at": "2026-09-17T09:00:00.123456Z",
		"applied_model": "env-model", "applied_thinking_level": "", "applied_service_tier": "",
		"applied_context_args": []any{},
	}
	if !reflect.DeepEqual(run.posts[0], want) {
		t.Fatalf("post = %v, want %v", run.posts[0], want)
	}
	if !strings.Contains(run.argv, "--model env-model") {
		t.Fatalf("launch argv %q does not use the reported model", run.argv)
	}
}

// R4: a tier the daemon drops is reported empty, never as requested.
func TestReportAppliedSettings_R4_DroppedValuesReportedEmpty(t *testing.T) {
	run := runAppliedTask(t, http.StatusOK, true, "", nil,
		AgentData{Model: "claude-opus-4-7", ServiceTier: "fast", ThinkingLevel: "not-a-level"})

	if len(run.posts) != 1 {
		t.Fatalf("posts = %v, want 1", run.posts)
	}
	got := run.posts[0]
	if got["applied_model"] != "claude-opus-4-7" || got["applied_service_tier"] != "" || got["applied_thinking_level"] != "" {
		t.Fatalf("post = %v, want the dropped tier and level reported empty", got)
	}
}

// F1: context args come from every launch input, in argv order.
func TestReportAppliedSettings_ContextArgsFromDefaultsAndCustom(t *testing.T) {
	run := runAppliedTask(t, http.StatusOK, true, "claude-opus-4-7", []string{"--autocompact", "200k"},
		AgentData{CustomArgs: []string{"--autocompact", "300k"}})

	if len(run.posts) != 1 {
		t.Fatalf("posts = %v, want 1", run.posts)
	}
	want := []any{"--autocompact", "200k", "--autocompact", "300k"}
	if got := run.posts[0]["applied_context_args"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("applied_context_args = %v, want %v", got, want)
	}
}

func TestLaunchContextArgs_PrefixDefaultsThenCustom(t *testing.T) {
	got := launchContextArgs("claude", []string{"--autocompact", "100k"}, []string{"--autocompact", "200k"}, []string{"--autocompact", "300k"})
	want := []string{"--autocompact", "100k", "--autocompact", "200k", "--autocompact", "300k"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("launchContextArgs = %v, want %v", got, want)
	}
	if got := launchContextArgs("claude", nil, nil, nil); got == nil || len(got) != 0 {
		t.Fatalf("no context args = %#v, want empty non-nil", got)
	}
}

// R5 daemon half: a claim that does not say supported is never reported.
func TestReportAppliedSettings_R5_UnsupportedClaimSendsNothing(t *testing.T) {
	run := runAppliedTask(t, http.StatusOK, false, "env-model", nil, AgentData{})

	if len(run.posts) != 0 {
		t.Fatalf("posted %v to a server that did not offer the endpoint", run.posts)
	}
	if run.argv == "" {
		t.Fatal("task did not launch")
	}
}

// R6: a refused or failing report is swallowed and the task still launches.
func TestReportAppliedSettings_R6_FailureDoesNotStopTheTask(t *testing.T) {
	for _, status := range []int{http.StatusConflict, http.StatusInternalServerError} {
		run := runAppliedTask(t, status, true, "env-model", nil, AgentData{})
		if len(run.posts) != 1 {
			t.Fatalf("status %d: posts = %d, want exactly one attempt", status, len(run.posts))
		}
		if run.argv == "" || run.result.Status != "completed" {
			t.Fatalf("status %d: task did not run to completion: argv=%q result=%+v", status, run.argv, run.result)
		}
	}
}

func TestCapabilities_AdvertiseAppliedSettings(t *testing.T) {
	for _, capability := range daemonCommonCapabilities() {
		if capability == "applied-settings-v1" {
			return
		}
	}
	t.Fatal("daemon does not advertise applied-settings-v1")
}
