package broker

import (
	"context"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// A replace ref in the cache must not swap the pinned commit's tree.
func TestSyntheticBaseIgnoresReplaceRefs(t *testing.T) {
	u := newUpstream(t)
	git(t, u.cache, "replace", u.c2, u.c3)
	got, err := MaterializeSyntheticBase(context.Background(), u.cache, u.c2, filepath.Join(t.TempDir(), "run"), "b")
	if err != nil {
		t.Fatal(err)
	}
	if got.Tree != u.tree2 {
		t.Fatalf("tree %s follows the replace ref, want pinned tree %s", got.Tree, u.tree2)
	}
}

// When the consumer exits early the producers must be stopped and reaped, not
// left blocked on a full pipe.
func TestSyntheticBaseCopyFailureDoesNotHang(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "w")
	cache := filepath.Join(root, "cache.git")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, work, "init", "-b", "main")
	blob := make([]byte, 4<<20) // incompressible, well past the pipe capacity
	if _, err := rand.Read(blob); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "big.bin"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", "-A")
	git(t, work, "commit", "-m", "big")
	sha := git(t, work, "rev-parse", "HEAD")
	if out, err := exec.Command("git", "clone", "--bare", work, cache).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shim := t.TempDir()
	script := "#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = index-pack ] && exit 1; done\nexec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	_, err = MaterializeSyntheticBase(ctx, cache, sha, filepath.Join(t.TempDir(), "run"), "b")
	if err == nil {
		t.Fatal("want error from failing index-pack")
	}
	if ctx.Err() != nil || time.Since(start) > 5*time.Second {
		t.Fatalf("copy hung until the context expired (%v)", time.Since(start))
	}
}

func TestIsSyntheticBase(t *testing.T) {
	u := newUpstream(t)
	build := func(t *testing.T) string {
		dest := filepath.Join(t.TempDir(), "repo")
		if _, err := MaterializeSyntheticBase(context.Background(), u.cache, u.c2, dest, "agent/x"); err != nil {
			t.Fatal(err)
		}
		return dest
	}
	check := func(t *testing.T, dest, sha, branch string) bool {
		return IsSyntheticBase(context.Background(), dest, sha, branch)
	}
	t.Run("built here, with local work on top", func(t *testing.T) {
		dest := build(t)
		if err := os.WriteFile(filepath.Join(dest, "local.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, dest, "add", "-A")
		git(t, dest, "-c", "user.name=a", "-c", "user.email=a@a", "commit", "-m", "local")
		if !check(t, dest, u.c2, "agent/x") {
			t.Fatal("a synthetic base with local commits was not recognised")
		}
	})
	t.Run("other sha or branch", func(t *testing.T) {
		dest := build(t)
		if check(t, dest, u.c3, "agent/x") || check(t, dest, u.c2, "agent/y") {
			t.Fatal("a mismatching pin or branch was accepted")
		}
	})
	t.Run("missing or empty destination", func(t *testing.T) {
		if check(t, filepath.Join(t.TempDir(), "none"), u.c2, "agent/x") || check(t, t.TempDir(), u.c2, "agent/x") {
			t.Fatal("a missing or empty destination was accepted")
		}
	})
	t.Run("a clone of the upstream", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "clone")
		git(t, "", "clone", "-q", "-b", "main", u.cache, dest)
		git(t, dest, "branch", "agent/x", u.c2)
		if check(t, dest, u.c2, "agent/x") {
			t.Fatal("an upstream clone was accepted")
		}
	})
	t.Run("a remote", func(t *testing.T) {
		dest := build(t)
		git(t, dest, "remote", "add", "origin", u.cache)
		if check(t, dest, u.c2, "agent/x") {
			t.Fatal("a repository with a remote was accepted")
		}
	})
	t.Run("alternates", func(t *testing.T) {
		dest := build(t)
		alt := filepath.Join(dest, ".git", "objects", "info", "alternates")
		if err := os.WriteFile(alt, []byte(filepath.Join(u.cache, "objects")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if check(t, dest, u.c2, "agent/x") {
			t.Fatal("a repository with alternates was accepted")
		}
	})
	t.Run(".git pointer file", func(t *testing.T) {
		dest := build(t)
		other := build(t)
		if err := os.RemoveAll(filepath.Join(dest, ".git")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dest, ".git"), []byte("gitdir: "+filepath.Join(other, ".git")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if check(t, dest, u.c2, "agent/x") {
			t.Fatal("a gitdir pointer was accepted")
		}
	})
}

// A matching synthetic branch does not prove the rest of a pre-existing
// repository is free of upstream history (review round 3).
func TestIsSyntheticBaseRejectsUpstreamHistory(t *testing.T) {
	u := newUpstream(t)
	foreignWithSyntheticBranch := func(t *testing.T) string {
		safe := filepath.Join(t.TempDir(), "safe")
		if _, err := MaterializeSyntheticBase(context.Background(), u.cache, u.c2, safe, "agent/x"); err != nil {
			t.Fatal(err)
		}
		foreign := filepath.Join(t.TempDir(), "foreign")
		git(t, "", "clone", "-q", u.cache, foreign)
		git(t, foreign, "remote", "remove", "origin")
		git(t, foreign, "fetch", "-q", safe, "refs/heads/agent/x:refs/heads/agent/x")
		git(t, foreign, "checkout", "-q", "agent/x")
		return foreign
	}
	t.Run("upstream branches and tags remain", func(t *testing.T) {
		foreign := foreignWithSyntheticBranch(t)
		if !strings.Contains(git(t, foreign, "log", "--all", "--format=%B"), upstreamMsg) {
			t.Fatal("invalid fixture: upstream history missing")
		}
		if IsSyntheticBase(context.Background(), foreign, u.c2, "agent/x") {
			t.Fatal("accepted upstream refs next to a synthetic branch")
		}
	})
	t.Run("upstream refs deleted, objects remain", func(t *testing.T) {
		foreign := foreignWithSyntheticBranch(t)
		git(t, foreign, "branch", "-D", "main")
		git(t, foreign, "tag", "-d", "upstream-tag")
		git(t, foreign, "reflog", "expire", "--expire=now", "--all")
		if strings.Contains(git(t, foreign, "log", "--all", "--format=%B"), upstreamMsg) {
			t.Fatal("invalid fixture: upstream still reachable")
		}
		if IsSyntheticBase(context.Background(), foreign, u.c2, "agent/x") {
			t.Fatal("accepted an object store that still holds upstream commits")
		}
	})
	t.Run("replace ref", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "repo")
		if _, err := MaterializeSyntheticBase(context.Background(), u.cache, u.c2, dest, "agent/x"); err != nil {
			t.Fatal(err)
		}
		root := git(t, dest, "rev-parse", "HEAD")
		git(t, dest, "update-ref", "refs/replace/"+root, root)
		if IsSyntheticBase(context.Background(), dest, u.c2, "agent/x") {
			t.Fatal("accepted a repository with a replace ref")
		}
	})
	t.Run("local branch and amended commit are fine", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "repo")
		if _, err := MaterializeSyntheticBase(context.Background(), u.cache, u.c2, dest, "agent/x"); err != nil {
			t.Fatal(err)
		}
		cfg := []string{"-c", "user.name=a", "-c", "user.email=a@a"}
		git(t, dest, "checkout", "-q", "-b", "scratch")
		if err := os.WriteFile(filepath.Join(dest, "n.txt"), []byte("n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, dest, "add", "-A")
		git(t, dest, append(cfg, "commit", "-m", "one")...)
		git(t, dest, append(cfg, "commit", "--amend", "-m", "two")...)
		git(t, dest, "tag", "mine")
		if !IsSyntheticBase(context.Background(), dest, u.c2, "agent/x") {
			t.Fatal("legitimate local branches, tags and amended commits were refused")
		}
	})
}
