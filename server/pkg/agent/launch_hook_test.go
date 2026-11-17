//go:build !windows

package agent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordedLaunch struct {
	path string
	env  []string
	dir  string
}

type recordingHook struct {
	mu    sync.Mutex
	calls []recordedLaunch
	err   error
}

func (h *recordingHook) Wrap(cmd *exec.Cmd) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, recordedLaunch{cmd.Path, append([]string(nil), cmd.Env...), cmd.Dir})
	return h.err
}

func (h *recordingHook) snapshot() []recordedLaunch {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]recordedLaunch(nil), h.calls...)
}

// countingExecutable appends one line to marker each time it is started.
func countingExecutable(t *testing.T, dir, name, marker string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	writeTestExecutable(t, p, []byte("#!/bin/sh\necho x >> '"+marker+"'\n"))
	return p
}

func markerCount(t *testing.T, marker string) int {
	t.Helper()
	b, err := os.ReadFile(marker)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}

func drain(t *testing.T, b Backend, opts ExecOptions) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := b.Execute(ctx, "p", opts)
	if err != nil {
		return // a refused start is a valid outcome for the error-path test
	}
	go func() {
		for range s.Messages {
		}
	}()
	<-s.Result
}

// AC1: every process started for a task passes the hook once, with the final
// Env and Dir.
func TestLaunchHookSeesEveryStartedProcess(t *testing.T) {
	for _, kind := range []string{"claude", "codex", "script"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "started")
			exe := countingExecutable(t, dir, "fake-"+kind, marker)
			cwd := t.TempDir()
			hook := &recordingHook{}
			cfg := Config{
				ExecutablePath: exe,
				Logger:         slog.Default(),
				TaskID:         "task-1",
				Env:            map[string]string{"HOOK_PROBE": "1"},
				Launch:         hook,
			}
			agentType := kind
			if kind == "script" {
				agentType = "script"
				sdir := t.TempDir()
				writeTestExecutable(t, filepath.Join(sdir, "bin"), []byte("#!/bin/sh\necho x >> '"+marker+"'\n"))
				if err := os.WriteFile(filepath.Join(sdir, "s.json"), []byte(`{"executable":"`+filepath.Join(sdir, "bin")+`","arg_patterns":[]}`), 0o600); err != nil {
					t.Fatal(err)
				}
				cfg.Script = &ScriptSpec{Dir: sdir, Name: "s"}
			}
			b, err := New(agentType, cfg)
			if err != nil {
				t.Fatal(err)
			}
			drain(t, b, ExecOptions{Cwd: cwd})

			started := markerCount(t, marker)
			calls := hook.snapshot()
			if started == 0 {
				t.Fatal("fake executable never started")
			}
			if len(calls) != started {
				t.Fatalf("hook calls = %d, processes started = %d", len(calls), started)
			}
			for _, c := range calls {
				if c.dir != cwd {
					t.Errorf("hook Dir = %q, want %q", c.dir, cwd)
				}
				if !containsEnv(c.env, "HOOK_PROBE=1") {
					t.Errorf("hook Env missing HOOK_PROBE=1: %v", c.env)
				}
			}
		})
	}
}

func containsEnv(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

// AC3: a hook error is returned and the process never starts.
func TestLaunchHookErrorPreventsStart(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "started")
	hookErr := errors.New("hook refused")
	hook := &recordingHook{err: hookErr}
	cmd := NewCommand(countingExecutable(t, dir, "fake", marker), nil)
	cmd.hook = hook
	ctx := context.Background()
	c := cmd.exec(ctx)
	if err := runOwned(c, slog.Default()); !errors.Is(err, hookErr) {
		t.Fatalf("runOwned err = %v, want %v", err, hookErr)
	}
	if n := markerCount(t, marker); n != 0 {
		t.Fatalf("process started %d times despite hook error", n)
	}
	if len(launchHookEntries()) != 0 {
		t.Fatal("lookup still holds the command after a hook error")
	}
}

// AC4: hook + CredentialBoundary is refused with a named error.
func TestLaunchHookWithCredentialBoundaryRefused(t *testing.T) {
	_, err := New("claude", Config{Launch: &recordingHook{}, RequireCredentialIsolation: true})
	if !errors.Is(err, ErrLaunchHookCredentialBoundary) {
		t.Fatalf("err = %v, want ErrLaunchHookCredentialBoundary", err)
	}
}

// AC6: a built command whose context is cancelled without a start leaves no
// entry in the lookup.
func TestLaunchHookEntryDroppedOnCancelWithoutStart(t *testing.T) {
	cmd := NewCommand("/bin/true", nil)
	cmd.hook = &recordingHook{}
	ctx, cancel := context.WithCancel(context.Background())
	_ = cmd.exec(ctx)
	if len(launchHookEntries()) != 1 {
		t.Fatalf("entries = %d, want 1 before cancel", len(launchHookEntries()))
	}
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for len(launchHookEntries()) != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(launchHookEntries()); n != 0 {
		t.Fatalf("entries = %d after cancel, want 0", n)
	}
}

// A Config with no hook registers nothing.
func TestNilLaunchHookRegistersNothing(t *testing.T) {
	_ = NewCommand("/bin/true", nil).exec(context.Background())
	if n := len(launchHookEntries()); n != 0 {
		t.Fatalf("entries = %d, want 0", n)
	}
}

// AC5: only launch.go and script.go may construct a process. A new backend that
// calls exec.Command would skip the launch hook.
func TestOnlyLaunchGoSpawnsRuntimeProcesses(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "launch.go" || f == "script.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			code, _, _ := strings.Cut(line, "//")
			if strings.Contains(code, "exec.Command(") || strings.Contains(code, "exec.CommandContext(") {
				t.Errorf("%s:%d calls exec.Command*; route it through Command.exec (launch.go)", f, i+1)
			}
		}
	}
}

// Review round 1 P1: the Hermes ACP terminal/create path starts task commands
// too. Every branch (argv, shell) must pass the hook, and the runtime launch
// prefix must not leak into them.
func TestLaunchHookSeesACPTerminalCommands(t *testing.T) {
	for _, params := range []string{
		`{"command":"/bin/echo","args":["hi"]}`,
		`{"command":"echo hi"}`,
	} {
		hook := &recordingHook{}
		cwd := t.TempDir()
		c := &hermesClient{
			cfg:         Config{Launch: hook, LaunchPrefix: []string{"--runtime-prefix"}},
			terminalCtx: context.Background(),
			terminalCwd: cwd,
			terminalEnv: os.Environ(),
			terminals:   make(map[string]*acpTerminal),
		}
		created, err := c.acpTerminalCreate([]byte(params))
		if err != nil {
			t.Fatalf("%s: create: %v", params, err)
		}
		id := created["terminalId"].(string)
		if _, err := c.acpTerminalResponse("terminal/wait_for_exit", []byte(`{"terminalId":"`+id+`"}`)); err != nil {
			t.Fatal(err)
		}
		calls := hook.snapshot()
		if len(calls) != 1 {
			t.Fatalf("%s: hook calls = %d, want 1", params, len(calls))
		}
		if calls[0].dir != cwd {
			t.Errorf("%s: hook Dir = %q, want %q", params, calls[0].dir, cwd)
		}
	}
}

// Review round 1 P2: a context that is already cancelled at registration must
// not leave an entry behind (cleanup must not run before the store).
func TestLaunchHookEntryDroppedWhenContextAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 20000; i++ {
		cmd := NewCommand("/bin/true", nil)
		cmd.hook = &recordingHook{}
		_ = cmd.exec(ctx)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(launchHookEntries()) != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(launchHookEntries()); n != 0 {
		t.Fatalf("entries = %d with an already-cancelled context, want 0", n)
	}
}
