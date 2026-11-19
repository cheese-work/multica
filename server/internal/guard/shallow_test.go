package guard

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitT(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("git %v: %s: %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}

func guardedGit(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out, errb bytes.Buffer
	e := &Engine{Policy: DefaultPolicy(), Runner: ExecRunner{Stdout: &out, Stderr: &errb}, Out: &out, Err: &errb}
	code, err := e.Exec(context.Background(), "git", args)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	return code, errb.String()
}

// A linked worktree shares the cache's `shallow` file, so a shallow fetch from
// a worktree shallows the shared cache (CHE-7286). The guard must stop it
// before git runs, so the cache is never shallow, not just healed later.
func TestShallowFetchInLinkedWorktreeIsRefused(t *testing.T) {
	src := t.TempDir()
	gitT(t, "init", src)
	for _, n := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(src, n), []byte(n), 0o644); err != nil {
			t.Fatal(err)
		}
		gitT(t, "-C", src, "add", n)
		gitT(t, "-C", src, "commit", "-m", n)
	}
	bare := filepath.Join(t.TempDir(), "cache.git")
	gitT(t, "clone", "--bare", "file://"+src, bare)
	wt := filepath.Join(t.TempDir(), "wt")
	gitT(t, "-C", bare, "worktree", "add", "--detach", wt, "HEAD")

	for _, flag := range []string{"--depth=1", "--deepen=1", "--shallow-since=2000-01-01", "--shallow-exclude=main"} {
		code, errs := guardedGit(t, "-C", wt, "fetch", flag, "origin", "HEAD")
		if code != 2 || !strings.Contains(errs, "shallow") {
			t.Errorf("%s: want refusal (exit 2, shallow line), got exit %d %q", flag, code, errs)
		}
		if got := gitT(t, "-C", bare, "rev-parse", "--is-shallow-repository"); got != "false" {
			t.Fatalf("%s: shared cache became shallow", flag)
		}
	}

	// The bare cache itself is shared by every worktree: same refusal.
	for _, flag := range []string{"--depth=1", "--deepen=1"} {
		code, errs := guardedGit(t, "-C", bare, "fetch", flag, "origin", "HEAD")
		if code != 2 || !strings.Contains(errs, "shallow") {
			t.Errorf("bare cache %s: want refusal, got exit %d %q", flag, code, errs)
		}
		if got := gitT(t, "-C", bare, "rev-parse", "--is-shallow-repository"); got != "false" {
			t.Fatalf("bare cache %s: cache became shallow", flag)
		}
	}

	// A normal fetch from the worktree is untouched.
	if code, errs := guardedGit(t, "-C", wt, "fetch", "origin", "HEAD"); code != 0 {
		t.Errorf("plain fetch refused: exit %d %q", code, errs)
	}
	// An independent clone has its own shallow file: allowed.
	clone := filepath.Join(t.TempDir(), "clone")
	gitT(t, "clone", "file://"+src, clone)
	if code, errs := guardedGit(t, "-C", clone, "fetch", "--depth=1", "origin", "HEAD"); code != 0 {
		t.Errorf("shallow fetch in a clone refused: exit %d %q", code, errs)
	}
}
