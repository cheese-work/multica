//go:build linux

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// wrapKill starts a fake agent that parks two processes, applies kill, then
// checks the run cgroup empties within WaitDelay of Wait returning.
func wrapKill(t *testing.T, kill func(cmd *exec.Cmd, cancel context.CancelFunc)) {
	t.Helper()
	l := newLauncher()
	requireSandbox(t, l)
	ly := newLayout(t)
	ready := filepath.Join(ly.work, "ready")
	agent := fakeAgent(t, ly.work, "sleep 300 &\nsleep 300 &\ntouch "+ready+"\nwait\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, agent)
	cmd.WaitDelay = 3 * time.Second
	run, err := l.Wrap(cmd, Spec{WorkDir: ly.work, TempDir: ly.tmp, Home: ly.home, NoDaemonAPI: true})
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	run.Started()
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if _, err := os.Stat(ready); err != nil {
		t.Fatal("fake agent never became ready")
	}
	procs := filepath.Join(run.scope.path, "cgroup.procs")
	if b, _ := os.ReadFile(procs); strings.TrimSpace(string(b)) == "" {
		t.Fatal("run cgroup is empty while the agent runs")
	}
	kill(cmd, cancel)
	_ = cmd.Wait()
	deadline := time.Now().Add(cmd.WaitDelay)
	for {
		b, _ := os.ReadFile(procs)
		if strings.TrimSpace(string(b)) == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("processes left in run cgroup after WaitDelay: %s", b)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestWrapContextCancelLeavesNoProcess(t *testing.T) {
	wrapKill(t, func(_ *exec.Cmd, cancel context.CancelFunc) { cancel() })
}

func TestWrapGroupSIGKILLLeavesNoProcess(t *testing.T) {
	wrapKill(t, func(cmd *exec.Cmd, _ context.CancelFunc) { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
}

func TestWrapGroupSIGTERMLeavesNoProcess(t *testing.T) {
	wrapKill(t, func(cmd *exec.Cmd, _ context.CancelFunc) { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) })
}
