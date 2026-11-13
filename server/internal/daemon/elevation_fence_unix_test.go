//go:build !windows

package daemon

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/processtree"
)

// T28 (j): a run holding a docker or device: scope. The fence ends its process
// group, children included, before it reports the launch scopes ended.
func TestRunFenceEndsProcessGroupBeforeLaunchScopes(t *testing.T) {
	// The shell backgrounds a child and waits; both hold the run's group.
	cmd := exec.Command("sh", "-c", "sleep 300 & echo $! ; wait")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- processtree.Run(runCtx, cmd, time.Second) }()

	buf := make([]byte, 32)
	n, err := out.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		t.Fatalf("child pid %q: %v", buf[:n], err)
	}

	var aliveAtReport = true
	f := RunFence{
		Broker:   fakeBroker{log: new([]string)},
		Restrict: func() {},
		StopRun:  func() error { cancel(); <-runDone; return nil },
		EndLaunchScopes: func(context.Context) error {
			aliveAtReport = !errors.Is(syscall.Kill(child, 0), syscall.ESRCH)
			return nil
		},
	}
	if err := f.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if aliveAtReport {
		t.Fatal("the run's child was still alive when launch scopes were reported ended")
	}
}
