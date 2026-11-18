package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fakeAgent writes an executable test script into dir and returns its path.
func fakeAgent(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "agent.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestWrapRunsFakeBinaryInSandbox(t *testing.T) {
	l := newLauncher()
	requireSandbox(t, l)
	ly := newLayout(t)
	sentinel := filepath.Join(ly.otherWork, "secret")
	agent := fakeAgent(t, ly.work, "echo \"task=$MULTICA_TASK_ID\"\ncat "+sentinel+" 2>&1 || echo unreadable\n")
	cmd := exec.Command(agent)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "MULTICA_TASK_ID=t1"}
	run, err := l.Wrap(cmd, Spec{WorkDir: ly.work, TempDir: ly.tmp, Home: ly.home})
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	out, err := cmd.CombinedOutput()
	run.Started()
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "task=t1") {
		t.Errorf("fake binary did not see its Env:\n%s", out)
	}
	if strings.Contains(string(out), "other-task") || !strings.Contains(string(out), "unreadable") {
		t.Errorf("sentinel outside the spec is readable:\n%s", out)
	}
}

func TestWrapRefusesExtraFilesAndForeignDir(t *testing.T) {
	l := newLauncher()
	ly := newLayout(t)
	spec := Spec{WorkDir: ly.work, TempDir: ly.tmp, Home: ly.home}
	agent := fakeAgent(t, ly.work, "true\n")

	withFiles := exec.Command(agent)
	withFiles.ExtraFiles = []*os.File{os.Stdin}
	if _, err := l.Wrap(withFiles, spec); err == nil {
		t.Error("Wrap accepted a command with ExtraFiles")
	}
	for _, dir := range []string{ly.otherWork, "relative", ly.work + "-sibling"} {
		cmd := exec.Command(agent)
		cmd.Dir = dir
		if _, err := l.Wrap(cmd, spec); err == nil {
			t.Errorf("Wrap accepted Dir %q outside WorkDir", dir)
		}
	}
}

func TestWrapSelfCheckFailureNeverStartsCommand(t *testing.T) {
	ly := newLayout(t)
	failing := fakeAgent(t, ly.root, "exit 1\n") // stands in for a bwrap that cannot run
	l := &Launcher{Mode: ModeOn, Logger: quiet(), BwrapPath: failing}
	marker := filepath.Join(ly.work, "ran")
	agent := fakeAgent(t, ly.work, "touch "+marker+"\n")
	cmd := exec.Command(agent)
	_, err := l.Wrap(cmd, Spec{WorkDir: ly.work, TempDir: ly.tmp, Home: ly.home})
	if !errors.Is(err, ErrSelfCheck) {
		t.Fatalf("Wrap error = %v, want ErrSelfCheck", err)
	}
	if cmd.Path != agent {
		t.Errorf("failed Wrap rewrote cmd.Path to %q", cmd.Path)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("command ran unsandboxed")
	}
}

func TestWrapOffAndAuditLeaveCommandUnchanged(t *testing.T) {
	ly := newLayout(t)
	agent := fakeAgent(t, ly.work, "true\n")
	for _, m := range []Mode{ModeOff, ModeAudit} {
		l := &Launcher{Mode: m, Logger: quiet()}
		cmd := exec.Command(agent, "a")
		cmd.Env = []string{"X=1"}
		cmd.Dir = ly.otherWork // audit logs this; off ignores it
		before := *cmd
		run, err := l.Wrap(cmd, Spec{WorkDir: ly.work, TempDir: ly.tmp, Home: ly.home})
		if err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		if !reflect.DeepEqual(*cmd, before) {
			t.Errorf("%s changed the command", m)
		}
		run.Started()
		if err := run.Close(); err != nil {
			t.Errorf("%s: Close: %v", m, err)
		}
	}
}

func TestWrapDirInsideWorkDirIsTheWorkingDirectory(t *testing.T) {
	l := newLauncher()
	requireSandbox(t, l)
	ly := newLayout(t)
	sub := filepath.Join(ly.work, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(fakeAgent(t, ly.work, "pwd\n"))
	cmd.Dir = sub
	run, err := l.Wrap(cmd, Spec{WorkDir: ly.work, TempDir: ly.tmp, Home: ly.home})
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	out, err := cmd.Output()
	run.Started()
	if got := strings.TrimSpace(string(out)); err != nil || got != sub {
		t.Errorf("pwd = %q, %v; want %q", got, err, sub)
	}
}
