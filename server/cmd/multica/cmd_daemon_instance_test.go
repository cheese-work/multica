package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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

func TestDaemonInstanceShutdownDrain(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("test fixture uses the Linux flock command")
	}
	for _, operation := range []string{"restart", "stop", "restart-already-draining", "stop-already-draining"} {
		t.Run(operation, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", root)
			t.Setenv(cli.TaskConfigRootEnv, "")
			t.Setenv("MULTICA_SERVER_URL", "")
			profile := "che1463-drain-" + operation
			api := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
				_, _ = responseWriter.Write([]byte(`{}`))
			}))
			defer api.Close()
			if err := cli.SaveCLIConfigForProfile(cli.CLIConfig{Token: "test-only-token", ServerURL: api.URL}, profile); err != nil {
				t.Fatal(err)
			}
			lockPath := filepath.Join(daemonDirForProfile(profile), "daemon.lock")
			owner := exec.Command("flock", "-n", lockPath, "sh", "-c", "printf ready; read release")
			input, err := owner.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			output, err := owner.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := owner.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				_ = input.Close()
				_ = owner.Wait()
			}()
			ready := make([]byte, len("ready"))
			if _, err := io.ReadFull(output, ready); err != nil {
				t.Fatal(err)
			}
			pidPath := daemonPIDPathForProfile(profile)
			pid := strconv.Itoa(owner.Process.Pid)
			if err := os.WriteFile(pidPath, []byte(pid), 0o644); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", healthPortForProfile(profile)))
			if err != nil {
				t.Fatal(err)
			}
			shutdown := make(chan struct{})
			server := &http.Server{Handler: http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/shutdown" {
					responseWriter.WriteHeader(http.StatusOK)
					responseWriter.(http.Flusher).Flush()
					close(shutdown)
					return
				}
				_ = json.NewEncoder(responseWriter).Encode(map[string]any{"status": "running", "profile": profile, "pid": owner.Process.Pid})
			})}
			go func() { _ = server.Serve(listener) }()
			defer server.Close()
			alreadyDraining := strings.Contains(operation, "already-draining")
			if alreadyDraining {
				_ = server.Close()
			}
			original := daemonExecutable
			replacement := errors.New("replacement launch reached")
			daemonExecutable = func() (string, error) {
				if err := exec.Command("flock", "-n", lockPath, "true").Run(); err != nil {
					return "", fmt.Errorf("replacement raced the draining owner: %w", err)
				}
				return "", replacement
			}
			defer func() { daemonExecutable = original }()
			result := make(chan error, 1)
			finished := make(chan struct{})
			command := newRestartTestCmd(t, profile)
			go func() {
				defer close(finished)
				if strings.HasPrefix(operation, "restart") {
					result <- runDaemonRestart(command, nil)
				} else {
					result <- runDaemonStop(command, nil)
				}
			}()
			defer func() {
				_ = input.Close()
				select {
				case <-finished:
				case <-time.After(40 * time.Second):
					t.Error("isolated lifecycle command did not finish")
				}
			}()
			if !alreadyDraining {
				select {
				case <-shutdown:
				case err := <-result:
					t.Fatalf("shutdown was not requested: %v", err)
				case <-time.After(5 * time.Second):
					t.Fatal("shutdown was not requested")
				}
				_ = server.Close()
			}
			returnedDuringDrain := false
			select {
			case err := <-result:
				returnedDuringDrain = true
				t.Errorf("%s returned before the owner released its lock: %v", operation, err)
			case <-time.After(time.Second):
			}
			contents, err := os.ReadFile(pidPath)
			if err != nil || string(contents) != pid {
				t.Errorf("%s changed the draining owner's PID file: %q, %v", operation, contents, err)
			}
			_ = input.Close()
			if returnedDuringDrain {
				return
			}
			select {
			case err := <-result:
				if strings.HasPrefix(operation, "restart") {
					if !errors.Is(err, replacement) {
						t.Errorf("restart did not reach a replacement after drain: %v", err)
					}
				} else if err != nil {
					t.Errorf("stop after drain: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("lifecycle command did not finish after lock release")
			}
			contents, err = os.ReadFile(pidPath)
			if err != nil || string(contents) != pid {
				t.Errorf("%s removed the owner's PID file after lock release: %q, %v", operation, contents, err)
			}
		})
	}
}
