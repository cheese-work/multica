//go:build linux

package runbinding

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
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
