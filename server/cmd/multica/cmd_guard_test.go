package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGH is a shell script standing in for the real gh. It records its calls.
const fakeGH = `#!/bin/sh
echo "$@" >> "$FAKE_GH_CALLS"
echo "real gh ran: $@"
exit ${FAKE_GH_EXIT:-0}
`

func runGuardCLI(t *testing.T, env map[string]string, args ...string) (code int, out, errs string) {
	t.Helper()
	dir := t.TempDir()
	gh := filepath.Join(dir, "gh")
	if err := os.WriteFile(gh, []byte(fakeGH), 0o755); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(dir, "calls")
	t.Setenv("MULTICA_GUARD_REAL_GH", gh)
	t.Setenv("FAKE_GH_CALLS", calls)
	t.Setenv("MULTICA_GUARD_LOG", filepath.Join(dir, "log.jsonl"))
	for k, v := range env {
		t.Setenv(k, v)
	}

	var o, e bytes.Buffer
	guardExecCmd.SetOut(&o)
	guardExecCmd.SetErr(&e)
	guardExecCmd.SetIn(strings.NewReader(""))
	defer func() { guardExecCmd.SetOut(nil); guardExecCmd.SetErr(nil); guardExecCmd.SetIn(nil) }()

	code = 0
	guardExit = func(c int) { code = c }
	defer func() { guardExit = os.Exit }()

	if err := runGuardExec(guardExecCmd, args); err != nil {
		t.Fatalf("runGuardExec: %v", err)
	}
	return code, o.String(), e.String()
}

func TestGuardExecPassesUnguardedCallsToRealTool(t *testing.T) {
	code, out, errs := runGuardCLI(t, nil, "gh", "issue", "list")
	if code != 0 || !strings.Contains(out, "real gh ran: issue list") || errs != "" {
		t.Errorf("exit %d out %q errs %q", code, out, errs)
	}
}

func TestGuardExecKeepsToolExitCode(t *testing.T) {
	code, _, _ := runGuardCLI(t, map[string]string{"FAKE_GH_EXIT": "3"}, "gh", "issue", "list")
	if code != 3 {
		t.Errorf("exit %d, want 3", code)
	}
}

func TestGuardExecRefusesAPIWriteToPulls(t *testing.T) {
	code, out, errs := runGuardCLI(t, nil, "gh", "api", "repos/o/r/pulls", "-X", "POST")
	if code != 2 || out != "" || !strings.Contains(errs, "guard:") || !strings.Contains(errs, "gh pr") {
		t.Errorf("exit %d out %q errs %q", code, out, errs)
	}
}

func TestGuardExecWritesDecisionLog(t *testing.T) {
	dir := t.TempDir()
	logf := filepath.Join(dir, "d.jsonl")
	runGuardCLI(t, map[string]string{"MULTICA_GUARD_LOG": logf}, "gh", "api", "repos/o/r/pulls", "-X", "POST")
	b, err := os.ReadFile(logf)
	if err != nil || !strings.Contains(string(b), `"action":"refused"`) {
		t.Errorf("log %q err %v", b, err)
	}
}
