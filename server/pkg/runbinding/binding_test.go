//go:build linux

package runbinding

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestBindingLifecycle(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p, err := New(ctx, Spec{dir, "agent-a", "task-a", "CHE-5868"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	path := filepath.Join(dir, p.ID+".json")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("published before launch")
	}
	cmd := exec.Command("/bin/cat")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { input.Close(); cmd.Wait() }()
	if err := p.Publish(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["pid"] != float64(cmd.Process.Pid) || got["principal"] != "agent-a" || got["task_id"] != "task-a" || got["binding_id"] != p.ID || got["process_start"] == "" || got["boot_id"] == "" || got["pid_namespace"] == "" {
		t.Fatalf("wrong identity: %s", data)
	}
	p.Close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("revoked record remains")
	}
	if err := p.Publish(cmd.Process.Pid); err == nil {
		t.Fatal("republished revoked binding")
	}
}

func TestBindingRejectsUnsafeIdentityAndCancellation(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	spec := Spec{dir, "agent", "task", "CHE-5868"}
	ctx, cancel := context.WithCancel(context.Background())
	p, err := New(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, pid := range []int{1, os.Getpid(), -1, 999999999} {
		if err := p.Publish(pid); err == nil {
			t.Fatalf("accepted borrowed/missing holder %d", pid)
		}
	}
	cancel()
	if err := p.Publish(os.Getpid()); err == nil {
		t.Fatal("published after cancellation")
	}
	if _, err := New(ctx, spec); err == nil {
		t.Fatal("created after cancellation")
	}
	os.Chmod(dir, 0755)
	if _, err := New(context.Background(), spec); err == nil {
		t.Fatal("accepted public registry")
	}
	link := filepath.Join(t.TempDir(), "registry")
	os.Symlink(dir, link)
	spec.Dir = link
	if _, err := New(context.Background(), spec); err == nil {
		t.Fatal("accepted symlink registry")
	}
}

// TestBindingNestedNamespace uses the same publisher as the native launcher.
// An optional Artemis probe runs the real consumer against synthetic inventory.
func TestBindingNestedNamespace(t *testing.T) {
	if out, err := exec.Command("unshare", "--user", "--map-root-user", "--pid", "--fork", "--mount-proc", "/bin/true").CombinedOutput(); err != nil {
		t.Skipf("namespace fixture unavailable: %v %s", err, out)
	}
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	p, err := New(context.Background(), Spec{dir, "agent", "run-1", "CHE-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	cmd := exec.Command("unshare", "--user", "--map-root-user", "--pid", "--fork", "--kill-child", "--mount-proc", "/bin/sh", "-c", `printf '%s %s\n' "$$" "$MULTICA_RUN_BINDING_ID"; read line`)
	cmd.Env = append(os.Environ(), "MULTICA_RUN_BINDING_ID="+p.ID)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { input.Close(); cmd.Wait() }()
	if err := p.Publish(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	visible, err := bufio.NewReader(output).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(visible) != "1 "+p.ID {
		t.Fatalf("namespace PID = %q, want 1", visible)
	}
	t.Logf("namespace PID 1 maps to launcher holder host PID %d", cmd.Process.Pid)
	if probe := os.Getenv("ARTEMIS_MANAGED_BINDING_PROBE"); probe != "" {
		database := filepath.Join(t.TempDir(), "synthetic.sqlite3")
		for _, phase := range []string{"active", "revoked"} {
			if phase == "revoked" {
				p.Close()
			}
			out, err := exec.Command("python3", "-B", probe, phase, dir, p.ID, database).CombinedOutput()
			if err != nil {
				t.Fatalf("consumer %s: %v %s", phase, err, out)
			}
			t.Log(strings.TrimSpace(string(out)))
		}
	}
}

func TestBindingCancellationRevokesAndAttemptsAreDistinct(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	spec := Spec{dir, "agent", "task", "CHE-5868"}
	p, err := New(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	other, err := New(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if p.ID == other.ID {
		t.Fatal("attempt IDs reused")
	}
	cmd := exec.Command("/bin/cat")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { input.Close(); cmd.Wait() }()
	if err := p.Publish(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	cancel()
	// Wait for the in-process cancellation callback, not a service or CI event.
	deadline := time.Now().Add(time.Second)
	for {
		_, err := os.Stat(filepath.Join(dir, p.ID+".json"))
		if os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancellation did not revoke binding while holder remained live")
		}
		runtime.Gosched()
	}
}
