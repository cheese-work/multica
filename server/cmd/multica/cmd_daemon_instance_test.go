package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

func TestDaemonInstanceBackgroundRejectsConcurrentWinner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test fixture uses a shell script")
	}
	root := t.TempDir()
	t.Setenv(cli.TaskConfigRootEnv, root)
	const profile = "che1463-start-race"
	if err := cli.SaveCLIConfigForProfile(cli.CLIConfig{Token: "test-only-token"}, profile); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", healthPortForProfile(profile)))
	if err != nil {
		t.Skipf("isolated profile health port unavailable: %v", err)
	}
	var probes atomic.Int32
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if probes.Add(1) == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "stopped"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "running", "profile": profile, "pid": os.Getpid()})
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	if err := os.MkdirAll(daemonDirForProfile(profile), 0o755); err != nil {
		t.Fatal(err)
	}
	pidPath := daemonPIDPathForProfile(profile)
	expectedPID := strconv.Itoa(os.Getpid())
	if err := os.WriteFile(pidPath, []byte(expectedPID), 0o644); err != nil {
		t.Fatal(err)
	}
	donePath := filepath.Join(root, "child-exited")
	t.Setenv("MULTICA_TEST_CHILD_DONE", donePath)
	executable := filepath.Join(root, "fake-daemon")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nsleep 1\nprintf done > \"$MULTICA_TEST_CHILD_DONE\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	original := daemonExecutable
	daemonExecutable = func() (string, error) { return executable, nil }
	t.Cleanup(func() { daemonExecutable = original })
	t.Cleanup(func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(donePath); err == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("fake daemon child did not exit")
	})
	command := &cobra.Command{Use: "start"}
	command.Flags().String("profile", profile, "")
	command.Flags().String("server-url", "", "")
	err = runDaemonBackground(command)
	if err == nil || !strings.Contains(err.Error(), "another daemon is already running") {
		t.Errorf("concurrent winner must not be reported as this child: %v", err)
	}
	pid, err := os.ReadFile(pidPath)
	if err != nil || string(pid) != expectedPID {
		t.Errorf("background launcher changed winner's PID file: %q, %v", pid, err)
	}
}
