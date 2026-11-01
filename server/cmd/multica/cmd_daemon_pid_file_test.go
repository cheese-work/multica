package main

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/daemon"
)

// CHE-1463: a foreground daemon publishes its port record and holds the
// profile lock; it writes no daemon.pid. A PID file outlives a crashed daemon
// and let a later stop signal whatever process reused that PID.
func TestDaemonForegroundWritesNoPIDFile(t *testing.T) {
	clearDaemonTaskEnv(t)
	const profile = "pid-file-test"
	mkProfiles(t, profile)
	originalSync := syncDaemonAutostart
	syncDaemonAutostart = func(string, bool) {}
	t.Cleanup(func() { syncDaemonAutostart = originalSync })

	ran := false
	pidFileWhileRunning := false
	originalRun := runDaemonWithHealthListener
	runDaemonWithHealthListener = func(_ *daemon.Daemon, _ context.Context, _ net.Listener) error {
		ran = true
		_, err := os.Stat(daemonPIDPathForProfile(profile))
		pidFileWhileRunning = !errors.Is(err, os.ErrNotExist)
		return context.Canceled
	}
	t.Cleanup(func() { runDaemonWithHealthListener = originalRun })

	cmd := &cobra.Command{Use: "start"}
	cmd.Flags().String("profile", profile, "")
	cmd.Flags().String("server-url", "http://127.0.0.1:1", "")
	if err := runDaemonForeground(cmd); err != nil {
		t.Fatalf("foreground start: %v", err)
	}
	if !ran {
		t.Fatal("foreground start never reached the daemon run loop")
	}
	if pidFileWhileRunning {
		t.Fatalf("foreground daemon wrote %s while running; CHE-1463 removed the PID file", daemonPIDPathForProfile(profile))
	}
}
