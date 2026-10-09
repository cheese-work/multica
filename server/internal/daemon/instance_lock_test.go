package daemon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
)

type instanceProcess struct {
	pid    int
	input  io.WriteCloser
	done   chan struct{}
	output bytes.Buffer
	err    error
}

func TestDaemonInstanceProcess(t *testing.T) {
	if os.Getenv("MULTICA_TEST_INSTANCE_PROCESS") != "1" {
		return
	}
	var start [1]byte
	if _, err := io.ReadFull(os.Stdin, start[:]); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	go func() {
		var command [1]byte
		if _, err := io.ReadFull(os.Stdin, command[:]); err == nil && command[0] == 'x' {
			os.Exit(0)
		}
		cancel()
	}()
	d := New(Config{
		Profile:        os.Getenv("MULTICA_TEST_INSTANCE_PROFILE"),
		ServerBaseURL:  os.Getenv("MULTICA_TEST_INSTANCE_SERVER"),
		WorkspacesRoot: os.Getenv("MULTICA_TEST_INSTANCE_WORKSPACES"),
		HealthPort:     0,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := d.Run(ctx); err != nil {
		fmt.Fprintln(os.Stdout, err)
		os.Exit(1)
	}
}

func instanceTestServer(t *testing.T) (string, <-chan struct{}) {
	t.Helper()
	entered := make(chan struct{}, 8)
	stop := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		entered <- struct{}{}
		select {
		case <-request.Context().Done():
		case <-stop:
		}
	}))
	t.Cleanup(func() {
		close(stop)
		server.Close()
	})
	return server.URL, entered
}

func instanceTestHome(t *testing.T, profiles ...string) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv(cli.TaskConfigRootEnv, root)
	for _, profile := range profiles {
		if err := cli.SaveCLIConfigForProfile(cli.CLIConfig{Token: "test-only-token"}, profile); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func startInstanceProcess(t *testing.T, root, serverURL, profile string) *instanceProcess {
	t.Helper()
	process := &instanceProcess{done: make(chan struct{})}
	command := exec.Command(os.Args[0], "-test.run=^TestDaemonInstanceProcess$")
	command.Env = append(os.Environ(),
		"MULTICA_TEST_INSTANCE_PROCESS=1",
		cli.TaskConfigRootEnv+"="+root,
		"MULTICA_TEST_INSTANCE_SERVER="+serverURL,
		"MULTICA_TEST_INSTANCE_PROFILE="+profile,
		"MULTICA_TEST_INSTANCE_WORKSPACES="+t.TempDir(),
	)
	command.Stdout = &process.output
	command.Stderr = &process.output
	var err error
	process.input, err = command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	process.pid = command.Process.Pid
	go func() {
		process.err = command.Wait()
		close(process.done)
	}()
	t.Cleanup(func() {
		_ = process.input.Close()
		select {
		case <-process.done:
		case <-time.After(20 * time.Second):
			t.Error("isolated daemon process did not exit")
		}
	})
	return process
}

func waitInstancePreflight(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("isolated daemon did not enter preflight")
	}
}

func TestDaemonInstanceConcurrentStarts(t *testing.T) {
	root := instanceTestHome(t, "")
	serverURL, entered := instanceTestServer(t)
	first := startInstanceProcess(t, root, serverURL, "")
	second := startInstanceProcess(t, root, serverURL, "")
	_, _ = first.input.Write([]byte("s"))
	_, _ = second.input.Write([]byte("s"))
	waitInstancePreflight(t, entered)
	var refused *instanceProcess
	select {
	case <-first.done:
		refused = first
	case <-second.done:
		refused = second
	case <-entered:
		t.Fatal("both concurrent daemon starts entered preflight")
	case <-time.After(5 * time.Second):
		t.Fatal("second daemon start was not refused")
	}
	if refused.err == nil || !strings.Contains(refused.output.String(), "daemon instance lock") {
		t.Fatalf("expected single-instance refusal, got %v: %s", refused.err, refused.output.String())
	}
	winner := first
	if refused == first {
		winner = second
	}
	pid, err := os.ReadFile(filepath.Join(root, "daemon.pid"))
	if err != nil || string(pid) != strconv.Itoa(winner.pid) {
		t.Fatalf("refused daemon changed the winner's PID file: %q, %v", pid, err)
	}
	select {
	case <-entered:
		t.Fatal("refused daemon also entered preflight")
	default:
	}
}

func TestDaemonInstanceStaleLockAfterProcessExit(t *testing.T) {
	root := instanceTestHome(t, "")
	serverURL, entered := instanceTestServer(t)
	first := startInstanceProcess(t, root, serverURL, "")
	_, _ = first.input.Write([]byte("s"))
	waitInstancePreflight(t, entered)
	_, _ = first.input.Write([]byte("x"))
	<-first.done
	if first.err != nil {
		t.Fatalf("isolated daemon exit: %v: %s", first.err, first.output.String())
	}
	lockPath := filepath.Join(root, "daemon.lock")
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock file must survive process death: %v", err)
	}
	second := startInstanceProcess(t, root, serverURL, "")
	_, _ = second.input.Write([]byte("s"))
	waitInstancePreflight(t, entered)
}

func TestDaemonInstanceIndependentProfiles(t *testing.T) {
	root := instanceTestHome(t, "", "other")
	serverURL, entered := instanceTestServer(t)
	first := startInstanceProcess(t, root, serverURL, "")
	second := startInstanceProcess(t, root, serverURL, "other")
	_, _ = first.input.Write([]byte("s"))
	_, _ = second.input.Write([]byte("s"))
	waitInstancePreflight(t, entered)
	waitInstancePreflight(t, entered)
}

func TestDaemonInstanceStartupFailureReleasesLock(t *testing.T) {
	instanceTestHome(t, "")
	if err := cli.SaveCLIConfig(cli.CLIConfig{}); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	for attempt := 0; attempt < 2; attempt++ {
		d := New(Config{HealthPort: port, WorkspacesRoot: t.TempDir()}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err := d.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "not authenticated") {
			t.Fatalf("attempt %d: expected auth failure after acquiring lock, got %v", attempt, err)
		}
	}
}

func TestDaemonInstanceHealthPortCollision(t *testing.T) {
	root := instanceTestHome(t, "")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	pidPath := filepath.Join(root, "daemon.pid")
	if err := os.WriteFile(pidPath, []byte("legacy-daemon"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := New(Config{HealthPort: listener.Addr().(*net.TCPAddr).Port}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := d.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "another daemon is already running on") {
		t.Fatalf("expected existing health-port collision, got %v", err)
	}
	pid, err := os.ReadFile(pidPath)
	if err != nil || string(pid) != "legacy-daemon" {
		t.Fatalf("health-port collision changed the existing PID file: %q, %v", pid, err)
	}
	lock, err := acquireDaemonInstanceLock("")
	if err != nil {
		t.Fatalf("health-port collision retained the instance lock: %v", err)
	}
	_ = lock.Close()
}
