package broker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const upstreamMsg = "UPSTREAM-PRIVATE-MESSAGE do not leak"

// upstream is a cache repository with three commits, a side branch and a tag.
type upstream struct {
	cache      string
	c1, c2, c3 string
	side       string
	tree2      string
}

func newUpstream(t *testing.T) upstream {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "w")
	cache := filepath.Join(root, "cache.git")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, work, "init", "-b", "main")
	write := func(name, body string, mode os.FileMode) {
		p := filepath.Join(work, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(msg string) string {
		git(t, work, "add", "-A")
		git(t, work, "commit", "-m", msg)
		return git(t, work, "rev-parse", "HEAD")
	}
	u := upstream{cache: cache}
	write("a.txt", "one\n", 0o644)
	u.c1 = commit(upstreamMsg + " 1")
	write("dir/run.sh", "#!/bin/sh\n", 0o755)
	write("a.txt", "two\n", 0o644)
	if err := os.Symlink("a.txt", filepath.Join(work, "link")); err != nil {
		t.Fatal(err)
	}
	u.c2 = commit(upstreamMsg + " 2")
	u.tree2 = git(t, work, "rev-parse", "HEAD^{tree}")
	git(t, work, "tag", "upstream-tag")
	git(t, work, "checkout", "-b", "side")
	write("side.txt", "side\n", 0o644)
	u.side = commit(upstreamMsg + " side")
	git(t, work, "checkout", "main")
	write("late.txt", "late\n", 0o644)
	u.c3 = commit(upstreamMsg + " 3")
	if out, err := exec.Command("git", "clone", "--bare", work, cache).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	return u
}

func TestSyntheticBaseExposesOnlyTheTree(t *testing.T) {
	u := newUpstream(t)
	dest := filepath.Join(t.TempDir(), "run")
	got, err := MaterializeSyntheticBase(context.Background(), u.cache, u.c2, dest, "agent/x/task-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Tree != u.tree2 || got.Source != u.c2 {
		t.Fatalf("result %+v, want tree %s source %s", got, u.tree2, u.c2)
	}
	if head := git(t, dest, "rev-parse", "HEAD"); head != got.Root {
		t.Fatalf("HEAD %s, want root %s", head, got.Root)
	}
	if tree := git(t, dest, "rev-parse", "HEAD^{tree}"); tree != u.tree2 {
		t.Fatalf("tree %s, want %s", tree, u.tree2)
	}

	// T27 Git part: git log --all shows only the synthetic root.
	log := git(t, dest, "log", "--all", "--format=%H|%an|%ae|%P|%B")
	if strings.Contains(log, "UPSTREAM") {
		t.Fatalf("log leaks upstream text: %q", log)
	}
	if lines := strings.Split(git(t, dest, "rev-list", "--all"), "\n"); len(lines) != 1 || lines[0] != got.Root {
		t.Fatalf("rev-list --all = %v, want only %s", lines, got.Root)
	}
	wantMsg := "base " + u.c2
	if msg := git(t, dest, "log", "-1", "--format=%s"); msg != wantMsg {
		t.Fatalf("message %q, want %q", msg, wantMsg)
	}
	if who := git(t, dest, "log", "-1", "--format=%an|%cn"); who != "multica-broker|multica-broker" {
		t.Fatalf("identity %q", who)
	}
	if parents := git(t, dest, "log", "-1", "--format=%P"); parents != "" {
		t.Fatalf("root has parents %q", parents)
	}

	// Every upstream commit object is absent.
	for _, sha := range []string{u.c1, u.c2, u.c3, u.side} {
		if out, err := exec.Command("git", "-C", dest, "cat-file", "-p", sha).CombinedOutput(); err == nil {
			t.Fatalf("upstream commit %s readable: %s", sha, out)
		}
	}
	// The only commit object in the whole object store is the root.
	objs := git(t, dest, "cat-file", "--batch-all-objects", "--batch-check=%(objecttype) %(objectname)")
	var commits []string
	for _, l := range strings.Split(objs, "\n") {
		if strings.HasPrefix(l, "commit ") {
			commits = append(commits, l)
		}
	}
	if len(commits) != 1 || commits[0] != "commit "+got.Root {
		t.Fatalf("commit objects %v, want only root", commits)
	}

	// No remote, no alternates, one branch, no tags, no hooks.
	if r := git(t, dest, "remote"); r != "" {
		t.Fatalf("remotes %q", r)
	}
	if _, err := os.Stat(filepath.Join(dest, ".git", "objects", "info", "alternates")); err == nil {
		t.Fatal("alternates present")
	}
	if refs := git(t, dest, "for-each-ref", "--format=%(refname)"); refs != "refs/heads/agent/x/task-1" {
		t.Fatalf("refs %q", refs)
	}
	if branch := git(t, dest, "symbolic-ref", "HEAD"); branch != "refs/heads/agent/x/task-1" {
		t.Fatalf("HEAD -> %q", branch)
	}
	if st := git(t, dest, "status", "--porcelain"); st != "" {
		t.Fatalf("dirty worktree: %q", st)
	}
	if hooks, _ := filepath.Glob(filepath.Join(dest, ".git", "hooks", "*")); len(hooks) != 0 {
		t.Fatalf("hooks present: %v", hooks)
	}
}

func TestSyntheticBaseWorktreeMatchesPinnedCommit(t *testing.T) {
	u := newUpstream(t)
	dest := filepath.Join(t.TempDir(), "run")
	if _, err := MaterializeSyntheticBase(context.Background(), u.cache, u.c2, dest, "b"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "a.txt")); string(b) != "two\n" {
		t.Fatalf("a.txt = %q", b)
	}
	if fi, err := os.Stat(filepath.Join(dest, "dir", "run.sh")); err != nil || fi.Mode()&0o111 == 0 {
		t.Fatalf("run.sh exec bit lost: %v %v", fi, err)
	}
	if target, err := os.Readlink(filepath.Join(dest, "link")); err != nil || target != "a.txt" {
		t.Fatalf("symlink %q %v", target, err)
	}
	// Files added by later upstream commits are not in the tree.
	for _, name := range []string{"late.txt", "side.txt"} {
		if _, err := os.Stat(filepath.Join(dest, name)); err == nil {
			t.Fatalf("%s leaked from a later commit", name)
		}
	}
}

func TestSyntheticBaseRootIsDeterministic(t *testing.T) {
	u := newUpstream(t)
	d := t.TempDir()
	a, err := MaterializeSyntheticBase(context.Background(), u.cache, u.c2, filepath.Join(d, "a"), "b")
	if err != nil {
		t.Fatal(err)
	}
	b, err := MaterializeSyntheticBase(context.Background(), u.cache, u.c2, filepath.Join(d, "b"), "b")
	if err != nil {
		t.Fatal(err)
	}
	if a.Root != b.Root {
		t.Fatalf("roots differ: %s vs %s", a.Root, b.Root)
	}
}

func TestSyntheticBaseRefusals(t *testing.T) {
	u := newUpstream(t)
	d := t.TempDir()
	occupied := filepath.Join(d, "occupied")
	if err := os.MkdirAll(occupied, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(occupied, "x"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, cache, sha, dest, branch string }{
		{"short sha", u.cache, u.c2[:12], filepath.Join(d, "1"), "b"},
		{"uppercase sha", u.cache, strings.ToUpper(u.c2), filepath.Join(d, "2"), "b"},
		{"option sha", u.cache, "--upload-pack=x", filepath.Join(d, "3"), "b"},
		{"empty sha", u.cache, "", filepath.Join(d, "4"), "b"},
		{"unknown sha", u.cache, strings.Repeat("a", 40), filepath.Join(d, "5"), "b"},
		{"bad branch", u.cache, u.c2, filepath.Join(d, "6"), "bad..name"},
		{"option branch", u.cache, u.c2, filepath.Join(d, "7"), "-x"},
		{"non-empty dest", u.cache, u.c2, occupied, "b"},
		{"missing cache", filepath.Join(d, "nope.git"), u.c2, filepath.Join(d, "8"), "b"},
		{"empty dest", u.cache, u.c2, "", "b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := MaterializeSyntheticBase(context.Background(), tc.cache, tc.sha, tc.dest, tc.branch); err == nil {
				t.Fatal("want error")
			}
		})
	}
	// A refusal leaves no half-built repository behind.
	if _, err := os.Stat(filepath.Join(d, "5")); err == nil {
		t.Fatal("failed materialize left its destination")
	}
}

func TestSyntheticBaseRejectsTreeOnlyShaThatIsNotACommit(t *testing.T) {
	u := newUpstream(t)
	if _, err := MaterializeSyntheticBase(context.Background(), u.cache, u.tree2, filepath.Join(t.TempDir(), "x"), "b"); err == nil {
		t.Fatal("tree SHA accepted as a pinned commit")
	}
}

func TestPinnedSourcePrefersHead(t *testing.T) {
	b, h := strings.Repeat("a", 40), strings.Repeat("b", 40)
	if got := PinnedSource(b, h); got != h {
		t.Fatalf("both set: %q, want head", got)
	}
	if got := PinnedSource(b, ""); got != b {
		t.Fatalf("base only: %q", got)
	}
	if got := PinnedSource("", h); got != h {
		t.Fatalf("head only: %q", got)
	}
	if got := PinnedSource("", ""); got != "" {
		t.Fatalf("neither: %q", got)
	}
}

func TestDiskUsageCountsFileBytes(t *testing.T) {
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "a"), make([]byte, 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(d, "s"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "s", "b"), make([]byte, 234), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a", filepath.Join(d, "l")); err != nil {
		t.Fatal(err)
	}
	got, err := DiskUsage(d)
	if err != nil {
		t.Fatal(err)
	}
	if got < 1234 || got > 1234+16384 {
		t.Fatalf("DiskUsage = %d", got)
	}
}

// M5 (plan section 5): at most 300 MB per elevated run for cheese-work/multica.
// Set MULTICA_M5_REPO to a git directory of that repository and
// MULTICA_M5_SHA to a commit on its default branch.
func TestM5SyntheticBaseSize(t *testing.T) {
	repo, sha := os.Getenv("MULTICA_M5_REPO"), os.Getenv("MULTICA_M5_SHA")
	if repo == "" || sha == "" {
		t.Skip("MULTICA_M5_REPO and MULTICA_M5_SHA not set")
	}
	dest := filepath.Join(t.TempDir(), "run")
	if _, err := MaterializeSyntheticBase(context.Background(), repo, sha, dest, "m5"); err != nil {
		t.Fatal(err)
	}
	n, err := DiskUsage(dest)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("M5: synthetic base %d bytes (%.1f MB)", n, float64(n)/1e6)
	if n > 300_000_000 {
		t.Fatalf("M5 miss: %d bytes > 300000000", n)
	}
}
