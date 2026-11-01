package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The cwd is part of Claude's system prompt, so a per-task cwd changes every
// byte after it and defeats the prompt cache. wrapStableCwd must make the
// child see the same cwd for different task dirs.
func TestWrapStableCwdShowsSameCwdForDifferentTaskDirs(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap is Linux-only")
	}
	if err := exec.Command("bwrap", "--dev-bind", "/", "/", "true").Run(); err != nil {
		t.Skipf("bwrap unusable here: %v", err)
	}
	root := t.TempDir()
	stable := filepath.Join(root, "stable")
	if err := os.Mkdir(stable, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, task := range []string{"task-a", "task-b"} {
		real := filepath.Join(root, task)
		if err := os.Mkdir(real, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(real, task+".marker"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", "-c", "pwd -P; ls")
		cmd.Dir = real
		if err := wrapStableCwd(cmd, real, stable); err != nil {
			t.Fatalf("%s: wrap: %v", task, err)
		}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: run: %v", task, err)
		}
		want := stable + "\n" + task + ".marker\n"
		if string(out) != want {
			t.Errorf("%s: output %q, want %q", task, out, want)
		}
	}
	if entries, _ := os.ReadDir(stable); len(entries) != 0 {
		t.Errorf("host mount point must stay empty, has %d entries", len(entries))
	}
}

func TestWrapStableCwdMissingBwrapErrors(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := wrapStableCwd(exec.Command("true"), "/a", "/b")
	if err == nil || !strings.Contains(err.Error(), "bwrap") {
		t.Fatalf("want bwrap lookup error, got %v", err)
	}
}
