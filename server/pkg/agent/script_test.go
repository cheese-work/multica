//go:build !windows

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testScriptToken = "mat_" + "0123456789abcdef" + "SECRETtail"

// scriptFixture is one allowlisted entry: an executable shell fixture plus its
// <name>.json entry. The fixture is a shell script only because the test needs
// something to run; the backend under test never invokes a shell.
type scriptFixture struct {
	dir    string
	marker string
}

func newScriptFixture(t *testing.T, name, body string, patterns []string) scriptFixture {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "bin-"+name)
	writeTestExecutable(t, exe, []byte("#!/bin/sh\n"+body+"\n"))
	entry, _ := json.Marshal(map[string]any{"executable": exe, "arg_patterns": patterns})
	if err := os.WriteFile(filepath.Join(dir, name+".json"), entry, 0o600); err != nil {
		t.Fatal(err)
	}
	return scriptFixture{dir: dir, marker: filepath.Join(dir, "ran")}
}

func runScript(t *testing.T, spec ScriptSpec, logger *slog.Logger) Result {
	t.Helper()
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	b, err := New("script", Config{
		Script: &spec,
		Logger: logger,
		TaskID: "task-1",
		Env: map[string]string{
			"MULTICA_TOKEN":            testScriptToken,
			"MULTICA_TASK_ID":          "task-1",
			"MULTICA_AGENT_ID":         "agent-1",
			"MULTICA_AUTOPILOT_RUN_ID": "run-1",
		},
	})
	if err != nil {
		t.Fatalf("New(script): %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := b.Execute(ctx, "", ExecOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	go func() {
		for range s.Messages {
		}
	}()
	return <-s.Result
}

func assertNotRan(t *testing.T, f scriptFixture) {
	t.Helper()
	if _, err := os.Stat(f.marker); err == nil {
		t.Fatal("process was started; marker file exists")
	}
}

func TestScriptUnknownNameFailsClosedWithoutProcess(t *testing.T) {
	t.Parallel()
	f := newScriptFixture(t, "known", `touch "$(dirname "$0")/ran"`, nil)
	for _, name := range []string{"ghost", "../known", "known/../known", ""} {
		res := runScript(t, ScriptSpec{Dir: f.dir, Name: name, Timeout: 5 * time.Second}, nil)
		if res.Status != "failed" || !strings.HasPrefix(res.Error, "script_not_allowlisted") {
			t.Fatalf("name %q: got status=%q error=%q", name, res.Status, res.Error)
		}
	}
	assertNotRan(t, f)
}

func TestScriptNoAllowlistDirFailsClosed(t *testing.T) {
	t.Parallel()
	for _, dir := range []string{"", filepath.Join(t.TempDir(), "missing")} {
		res := runScript(t, ScriptSpec{Dir: dir, Name: "anything", Timeout: 5 * time.Second}, nil)
		if res.Status != "failed" || !strings.HasPrefix(res.Error, "script_not_allowlisted") {
			t.Fatalf("dir %q: got status=%q error=%q", dir, res.Status, res.Error)
		}
	}
}

func TestScriptMalformedEntryFailsClosed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cases := map[string]string{
		"badjson":     `{`,
		"relative":    `{"executable":"bin/x","arg_patterns":[]}`,
		"noexec":      `{"arg_patterns":[]}`,
		"badpattern":  `{"executable":"/bin/true","arg_patterns":["("]}`,
		"unknownkeys": `{"executable":"/bin/true","arg_patterns":[],"shell":"sh -c"}`,
	}
	for name, body := range cases {
		if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		res := runScript(t, ScriptSpec{Dir: dir, Name: name, Timeout: 5 * time.Second}, nil)
		if res.Status != "failed" || !strings.HasPrefix(res.Error, "script_not_allowlisted") {
			t.Fatalf("%s: got status=%q error=%q", name, res.Status, res.Error)
		}
	}
}

func TestScriptRejectedArgsFailClosedWithoutProcess(t *testing.T) {
	t.Parallel()
	f := newScriptFixture(t, "tick", `touch "$(dirname "$0")/ran"`, []string{`--mode=(a|b)`})
	for _, args := range [][]string{
		{"--mode=c"},
		{"--mode=a", "extra"},
		{"--mode=a;touch x"},
		{"x--mode=a"}, // patterns match the whole argument, not a substring
	} {
		res := runScript(t, ScriptSpec{Dir: f.dir, Name: "tick", Args: args, Timeout: 5 * time.Second}, nil)
		if res.Status != "failed" || !strings.HasPrefix(res.Error, "script_args_rejected") {
			t.Fatalf("args %q: got status=%q error=%q", args, res.Status, res.Error)
		}
	}
	assertNotRan(t, f)
}

func TestScriptEntryWithoutPatternsAcceptsNoArgs(t *testing.T) {
	t.Parallel()
	f := newScriptFixture(t, "bare", `touch "$(dirname "$0")/ran"`, nil)
	res := runScript(t, ScriptSpec{Dir: f.dir, Name: "bare", Args: []string{"x"}, Timeout: 5 * time.Second}, nil)
	if !strings.HasPrefix(res.Error, "script_args_rejected") {
		t.Fatalf("got status=%q error=%q", res.Status, res.Error)
	}
	assertNotRan(t, f)
	if res := runScript(t, ScriptSpec{Dir: f.dir, Name: "bare", Timeout: 5 * time.Second}, nil); res.Status != "completed" {
		t.Fatalf("no-arg run: status=%q error=%q", res.Status, res.Error)
	}
}

func TestScriptSuccessEnvAndNoShell(t *testing.T) {
	t.Setenv("MULTICA_TEST_PARENT_LEAK", "leaked")
	f := newScriptFixture(t, "env", `
echo "task=$MULTICA_TASK_ID agent=$MULTICA_AGENT_ID run=$MULTICA_AUTOPILOT_RUN_ID"
[ "$MULTICA_TOKEN" = "`+testScriptToken+`" ] && echo token-present
echo "leak=${MULTICA_TEST_PARENT_LEAK:-none}"
echo "arg=$1"`, []string{`.*`})
	res := runScript(t, ScriptSpec{Dir: f.dir, Name: "env", Args: []string{"$(touch ran);x"}, Timeout: 5 * time.Second}, nil)
	if res.Status != "completed" {
		t.Fatalf("status=%q error=%q", res.Status, res.Error)
	}
	for _, want := range []string{"task=task-1 agent=agent-1 run=run-1", "token-present", "leak=none", "arg=$(touch ran);x"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("output missing %q:\n%s", want, res.Output)
		}
	}
	assertNotRan(t, f) // argument was passed literally, never interpreted by a shell
}

func TestScriptNonZeroFailsWithCappedRedactedTail(t *testing.T) {
	t.Parallel()
	f := newScriptFixture(t, "boom", `
i=0
while [ $i -lt 400 ]; do echo "line-$i padding-padding-padding-padding"; i=$((i+1)); done
echo "token=$MULTICA_TOKEN"
echo "other=mat_zzzzzzzzzzzzzzzz mdt_yyyyyyyyyyyyyyyy"
exit 3`, nil)
	var logs bytes.Buffer
	res := runScript(t, ScriptSpec{Dir: f.dir, Name: "boom", Timeout: 5 * time.Second}, slog.New(slog.NewTextHandler(&logs, nil)))
	if res.Status != "failed" || !strings.HasPrefix(res.Error, "script_failed") {
		t.Fatalf("status=%q error=%q", res.Status, res.Error)
	}
	if !strings.Contains(res.Error, "exit status 3") && !strings.Contains(res.Error, "exit code 3") {
		t.Fatalf("error should name the exit code: %q", res.Error)
	}
	if len(res.Error) > 4096 {
		t.Fatalf("tail not capped: %d bytes", len(res.Error))
	}
	if strings.Contains(res.Error, "line-0 ") || !strings.Contains(res.Error, "line-399") {
		t.Fatalf("expected the last lines only:\n%s", res.Error)
	}
	for _, secret := range []string{testScriptToken, "SECRETtail", "mat_zzzz", "mdt_yyyy"} {
		if strings.Contains(res.Error, secret) || strings.Contains(res.Output, secret) || strings.Contains(logs.String(), secret) {
			t.Fatalf("secret %q leaked\nerror=%s\nlogs=%s", secret, res.Error, logs.String())
		}
	}
}

func TestScriptTimeoutKillsProcessTree(t *testing.T) {
	t.Parallel()
	f := newScriptFixture(t, "slow", `
sleep 60 &
echo $! > "$(dirname "$0")/child.pid"
wait`, nil)
	start := time.Now()
	res := runScript(t, ScriptSpec{Dir: f.dir, Name: "slow", Timeout: 500 * time.Millisecond}, nil)
	if res.Status != "timeout" {
		t.Fatalf("status=%q error=%q", res.Status, res.Error)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("timeout took %s", time.Since(start))
	}
	raw, err := os.ReadFile(filepath.Join(f.dir, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d survived the timeout", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestScriptTokenNeverInDaemonLog(t *testing.T) {
	t.Parallel()
	f := newScriptFixture(t, "ok", `echo "$MULTICA_TOKEN"`, nil)
	var logs bytes.Buffer
	res := runScript(t, ScriptSpec{Dir: f.dir, Name: "ok", Timeout: 5 * time.Second}, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if res.Status != "completed" {
		t.Fatalf("status=%q error=%q", res.Status, res.Error)
	}
	if strings.Contains(res.Output, "SECRETtail") || strings.Contains(logs.String(), "SECRETtail") {
		t.Fatalf("token leaked\noutput=%s\nlogs=%s", res.Output, logs.String())
	}
}

func TestNewScriptRequiresSpec(t *testing.T) {
	t.Parallel()
	if _, err := New("script", Config{}); err == nil {
		t.Fatal(`New("script") without Config.Script must fail`)
	}
}

func TestScriptExitCode(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
		ok   bool
	}{
		{"script_failed: exit code 7\nlast line", 7, true},
		{"script_failed: did not start\n", 0, false},
		{"script_not_allowlisted: script name is not allowlisted", 0, false},
		{"script \"x\" timed out after 1s", 0, false},
		{"", 0, false},
	} {
		got, ok := ScriptExitCode(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ScriptExitCode(%q) = %d,%v want %d,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
