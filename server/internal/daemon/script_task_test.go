//go:build !windows

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const scriptTaskToken = "mat_" + "0123456789abcdef" + "SECRETtail"

type scriptTaskEnv struct {
	d      *Daemon
	allow  string // allowlist dir (HOME/.multica/scripts)
	starts *atomic.Int32
	logs   *bytes.Buffer
}

// newScriptTaskEnv points the daemon's allowlist at a temp HOME and counts
// /start calls. cfg.Agents stays empty: a script task that tried to resolve a
// model provider would fail with "no agent configured".
func newScriptTaskEnv(t *testing.T) scriptTaskEnv {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MULTICA_TASK_CONFIG_ROOT", "")
	var starts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/start") {
			starts.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	logs := &bytes.Buffer{}
	d := &Daemon{
		client:         NewClient(srv.URL),
		logger:         slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		workspaces:     make(map[string]*workspaceState),
		runtimeIndex:   map[string]Runtime{"rt-1": {ID: "rt-1", Provider: "claude"}},
		activeEnvRoots: make(map[string]int),
		cfg:            Config{ScriptTimeout: 5 * time.Second, WorkspacesRoot: t.TempDir()},
	}
	return scriptTaskEnv{d: d, allow: filepath.Join(home, ".multica", "scripts"), starts: &starts, logs: logs}
}

func (e scriptTaskEnv) allowScript(t *testing.T, name, body string, patterns []string) (marker string) {
	t.Helper()
	if err := os.MkdirAll(e.allow, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(e.allow, "bin-"+name)
	writeTestExecutable(t, exe, []byte("#!/bin/sh\n"+body+"\n"))
	entry, _ := json.Marshal(map[string]any{"executable": exe, "arg_patterns": patterns})
	if err := os.WriteFile(filepath.Join(e.allow, name+".json"), entry, 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(e.allow, "ran")
}

func scriptTask(name string, args ...string) Task {
	return Task{
		ID: "task-s1", WorkspaceID: "ws-1", RuntimeID: "rt-1", AgentID: "agent-1",
		Agent:          &AgentData{ID: "agent-1", Name: "script-agent"},
		AuthToken:      scriptTaskToken,
		AutopilotID:    "ap-1",
		AutopilotRunID: "run-1",
		Script:         &TaskScript{Name: name, Args: args},
	}
}

func (e scriptTaskEnv) run(t *testing.T, task Task) (TaskResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return e.d.runTask(ctx, task, "claude", 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func assertBlocked(t *testing.T, res TaskResult, err error, reason string) {
	t.Helper()
	if err != nil {
		t.Fatalf("runTask error: %v", err)
	}
	if res.Status != "blocked" || res.FailureReason != reason {
		t.Fatalf("got status=%q reason=%q comment=%q; want blocked/%s", res.Status, res.FailureReason, res.Comment, reason)
	}
}

func assertNoToken(t *testing.T, e scriptTaskEnv, res TaskResult) {
	t.Helper()
	for _, s := range []string{res.Comment, res.FailureReason, e.logs.String()} {
		if strings.Contains(s, "SECRETtail") || strings.Contains(s, scriptTaskToken) {
			t.Fatalf("token leaked: %q", s)
		}
	}
}

func TestScriptTaskNoAllowlistFailsClosed(t *testing.T) {
	e := newScriptTaskEnv(t) // no ~/.multica/scripts at all
	res, err := e.run(t, scriptTask("anything"))
	assertBlocked(t, res, err, "script_not_allowlisted")
	assertNoToken(t, e, res)
}

func TestScriptTaskUnknownNameAndRejectedArgsStartNoProcess(t *testing.T) {
	e := newScriptTaskEnv(t)
	marker := e.allowScript(t, "tick", `touch "$(dirname "$0")/ran"`, []string{`--mode=(a|b)`})
	res, err := e.run(t, scriptTask("ghost"))
	assertBlocked(t, res, err, "script_not_allowlisted")
	res, err = e.run(t, scriptTask("tick", "--mode=c"))
	assertBlocked(t, res, err, "script_args_rejected")
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("process was started")
	}
}

func TestScriptTaskSuccessRunsWithTaskEnvAndNoModel(t *testing.T) {
	e := newScriptTaskEnv(t)
	e.allowScript(t, "tick", `
echo "task=$MULTICA_TASK_ID agent=$MULTICA_AGENT_ID run=$MULTICA_AUTOPILOT_RUN_ID"
[ "$MULTICA_TOKEN" = "`+scriptTaskToken+`" ] && echo token-present
echo "token=$MULTICA_TOKEN"`, []string{`--mode=a`})
	res, err := e.run(t, scriptTask("tick", "--mode=a"))
	if err != nil || res.Status != "completed" {
		t.Fatalf("status=%q err=%v comment=%q", res.Status, err, res.Comment)
	}
	for _, want := range []string{"task=task-s1 agent=agent-1 run=run-1", "token-present"} {
		if !strings.Contains(res.Comment, want) {
			t.Fatalf("comment missing %q:\n%s", want, res.Comment)
		}
	}
	if e.starts.Load() != 1 {
		t.Fatalf("StartTask calls = %d, want 1", e.starts.Load())
	}
	assertNoToken(t, e, res)
}

func TestScriptTaskNonZeroExitFailsWithTail(t *testing.T) {
	e := newScriptTaskEnv(t)
	e.allowScript(t, "boom", `echo "about to fail with $MULTICA_TOKEN"; echo last-line; exit 7`, nil)
	res, err := e.run(t, scriptTask("boom"))
	assertBlocked(t, res, err, "script_failed")
	if !strings.Contains(res.Comment, "exit code 7") || !strings.Contains(res.Comment, "last-line") {
		t.Fatalf("comment lacks exit code or tail: %q", res.Comment)
	}
	assertNoToken(t, e, res)
}

func TestScriptTaskTimeoutFailsTask(t *testing.T) {
	e := newScriptTaskEnv(t)
	e.d.cfg.ScriptTimeout = 300 * time.Millisecond
	e.allowScript(t, "slow", `sleep 30`, nil)
	start := time.Now()
	res, err := e.run(t, scriptTask("slow"))
	assertBlocked(t, res, err, "timeout")
	if time.Since(start) > 10*time.Second {
		t.Fatalf("timeout took %s", time.Since(start))
	}
}

func TestScriptTaskRefusesNonTaskToken(t *testing.T) {
	e := newScriptTaskEnv(t)
	marker := e.allowScript(t, "tick", `touch "$(dirname "$0")/ran"`, nil)
	for _, tok := range []string{"", "mul_personalaccesstoken", "mdt_daemontoken0000"} {
		task := scriptTask("tick")
		task.AuthToken = tok
		if _, err := e.run(t, task); err == nil {
			t.Fatalf("token %q: want error", tok)
		}
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("process was started with a non-task token")
	}
	if e.starts.Load() != 0 {
		t.Fatalf("task started despite bad token: %d", e.starts.Load())
	}
}

// CHE-5094: an idle script tick runs no model, so its result carries no usage
// and the daemon has nothing to report to /usage.
func TestScriptTaskReportsZeroModelUsage(t *testing.T) {
	e := newScriptTaskEnv(t)
	e.allowScript(t, "tick", `echo idle`, nil)
	res, err := e.run(t, scriptTask("tick"))
	if err != nil || res.Status != "completed" {
		t.Fatalf("status=%q err=%v", res.Status, err)
	}
	if len(res.Usage) != 0 || res.ContextTokens != 0 {
		t.Fatalf("script result carries model usage: %+v", res)
	}
}
