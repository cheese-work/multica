package repocache

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/sandbox"
)

// TestSharedObjectsCheckoutWorksInsideSandbox runs git in the real bwrap
// profile: the task workdir and the cache objects dir (read-only) are the only
// repo-related mounts, and the cache's refs, config and worktree metadata plus
// the docker socket are not reachable. Skipped where the host cannot sandbox.
func TestSharedObjectsCheckoutWorksInsideSandbox(t *testing.T) {
	t.Parallel()
	l := &sandbox.Launcher{
		Mode: sandbox.ModeOn, Deny: sandbox.DefaultDeny(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	checkCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := l.SelfCheck(checkCtx); err != nil {
		t.Skipf("host cannot run the sandbox: %v", err)
	}

	f := newSharedFixture(t)
	res := f.checkout(t, sharedTaskA, true)
	tmp, home := t.TempDir(), t.TempDir()

	script := strings.Join([]string{
		`cd "$CHECKOUT"`,
		`echo head=$(git rev-parse HEAD)`,
		`git -c user.name=t -c user.email=t@t.test commit --allow-empty -qm sandboxed && echo commit-ok`,
		`git fsck --connectivity-only >/dev/null 2>&1 && echo fsck-ok`,
		`ls "$CACHE/config" "$CACHE/refs" "$CACHE/worktrees" /var/run/docker.sock /run/docker.sock 2>&1`,
		`touch "$CACHE/objects/new" 2>&1`,
	}, "\n")
	run, err := l.Command(context.Background(), sandbox.Spec{
		WorkDir:  f.workDir,
		TempDir:  tmp,
		Home:     home,
		ReadOnly: []string{ObjectsDir(f.barePath)},
		Env:      []string{"PATH=/usr/bin:/bin", "CHECKOUT=" + res.Path, "CACHE=" + f.barePath},
		Argv:     []string{"/bin/sh", "-c", script},
	})
	if err != nil {
		t.Fatalf("sandbox command: %v", err)
	}
	defer run.Close()
	var out bytes.Buffer
	run.Cmd.Stdout, run.Cmd.Stderr = &out, &out
	if err := run.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := run.Cmd.Wait(); err != nil {
		t.Logf("script exit: %v", err)
	}

	got := out.String()
	for _, want := range []string{"head=" + f.baseCommit, "commit-ok", "fsck-ok", "Read-only file system"} {
		if !strings.Contains(got, want) {
			t.Errorf("sandbox output missing %q:\n%s", want, got)
		}
	}
	// config, refs, worktrees, and both docker.sock paths: five "No such file".
	if n := strings.Count(got, "No such file"); n != 5 {
		t.Errorf("want 5 unreachable paths, got %d:\n%s", n, got)
	}
	if _, err := os.Stat(filepath.Join(f.barePath, "objects", "new")); err == nil {
		t.Error("sandbox wrote into the cache objects dir")
	}
}
