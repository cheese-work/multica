package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The daemon stops an output-only run at the run deadline the claim carries
// (context.run_deadline_seconds, CHE-4103), well before the daemon-wide
// AgentTimeout, and no provider process outlives it.
func TestRunTaskBackgroundStopsTheProcessAtTheRunDeadline(t *testing.T) {
	f := newBackgroundFixture(t) // AgentTimeout 5s
	pidFile := filepath.Join(t.TempDir(), "pid")
	script := "#!/bin/sh\n" +
		"case \"$*\" in *--version*) echo '2.1.318'; exit 0;; esac\n" +
		"IFS= read -r _\n" +
		"echo $$ > '" + pidFile + "'\n" +
		"exec sleep 30\n"
	writeTestExecutable(t, f.d.cfg.Agents["claude"].Path, []byte(script))

	task := backgroundTask()
	task.BackgroundRunSeconds = 1
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	start := time.Now()
	res, err := f.d.runTask(context.Background(), task, "claude", 0, logger)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "blocked" || res.FailureReason != "bg_run_timeout" {
		t.Fatalf("status/reason = %q/%q, want blocked/bg_run_timeout", res.Status, res.FailureReason)
	}
	if elapsed < time.Second || elapsed > 4*time.Second {
		t.Fatalf("run lasted %s, want about the 1 s run deadline (not the 5 s AgentTimeout)", elapsed)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("provider never started: %v", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatalf("provider process %d still alive after the run deadline", pid)
	}
}
