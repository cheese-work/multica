package repocache

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	sharedTaskA = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	sharedTaskB = "11111111-2222-3333-4444-555555555555"
)

// sharedFixture is one source repo, its cache and a task workdir.
type sharedFixture struct {
	cache      *Cache
	source     string
	barePath   string
	workDir    string
	baseCommit string
}

func newSharedFixture(t *testing.T) sharedFixture {
	t.Helper()
	source := createTestRepo(t)
	cache := New(t.TempDir(), testLogger())
	if err := cache.Sync("ws-1", []RepoInfo{{URL: source}}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	barePath := cache.Lookup("ws-1", source)
	return sharedFixture{
		cache: cache, source: source, barePath: barePath, workDir: t.TempDir(),
		baseCommit: gitRefCommit(t, barePath, getRemoteDefaultBranch(barePath)),
	}
}

func (f sharedFixture) checkout(t *testing.T, taskID string, shared bool) *WorktreeResult {
	t.Helper()
	res, err := f.cache.CreateWorktree(WorktreeParams{
		WorkspaceID: "ws-1", RepoURL: f.source, WorkDir: f.workDir,
		AgentName: "Sandboxed", TaskID: taskID, SharedObjects: shared,
	})
	if err != nil {
		t.Fatalf("CreateWorktree(%s): %v", taskID, err)
	}
	return res
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t.test"}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func hasObject(dir, sha string) bool {
	return exec.Command("git", "-C", dir, "cat-file", "-e", sha+"^{commit}").Run() == nil
}

// dropCacheRefsAndGC makes every cache object unreachable except through
// refs/multica/, then prunes them: what cache maintenance does once upstream
// drops a branch.
func dropCacheRefsAndGC(t *testing.T, barePath string) {
	t.Helper()
	refs := gitIn(t, barePath, "for-each-ref", "--format=%(refname)")
	for _, ref := range strings.Fields(refs) {
		if !strings.HasPrefix(ref, "refs/multica/") {
			gitIn(t, barePath, "update-ref", "-d", ref)
		}
	}
	gitIn(t, barePath, "reflog", "expire", "--expire=now", "--all")
	gitIn(t, barePath, "gc", "--prune=now")
}

func TestSharedObjectsCheckoutBorrowsCacheObjects(t *testing.T) {
	t.Parallel()
	f := newSharedFixture(t)
	res := f.checkout(t, sharedTaskA, true)

	gitDir := filepath.Join(res.Path, ".git")
	if info, err := os.Stat(gitDir); err != nil || !info.IsDir() {
		t.Fatalf(".git must be a standalone directory, info=%v err=%v", info, err)
	}
	alt, err := os.ReadFile(filepath.Join(gitDir, "objects", "info", "alternates"))
	if err != nil {
		t.Fatalf("read alternates: %v", err)
	}
	if got, want := strings.TrimSpace(string(alt)), ObjectsDir(f.barePath); got != want {
		t.Fatalf("alternates = %q, want %q", got, want)
	}
	if !isIsolatedCheckout(res.Path) {
		t.Fatal("shared-objects checkout must carry the isolated-checkout marker")
	}
	if got := gitHead(t, res.Path); got != f.baseCommit {
		t.Fatalf("HEAD = %s, want %s", got, f.baseCommit)
	}
	if err := runGit("-C", f.barePath, "show-ref", "--verify", "refs/heads/"+res.BranchName); err == nil {
		t.Fatalf("task branch %s leaked into the cache", res.BranchName)
	}
	if out, err := exec.Command("git", "-C", f.barePath, "worktree", "list", "--porcelain").Output(); err != nil || strings.Contains(string(out), res.Path) {
		t.Fatalf("cache lists the task checkout as a worktree (err=%v):\n%s", err, out)
	}

	// Borrowed objects stay in the cache; new ones land in the task's own store.
	gitIn(t, res.Path, "commit", "--allow-empty", "-m", "task work")
	work := gitHead(t, res.Path)
	if hasObject(f.barePath, work) {
		t.Fatal("new task commit was written into the cache object store")
	}
	if !hasObject(res.Path, work) {
		t.Fatal("new task commit is missing from the task checkout")
	}
}

func TestSharedObjectsKeepRefPinsBaseAgainstCacheGC(t *testing.T) {
	t.Parallel()
	f := newSharedFixture(t)
	res := f.checkout(t, sharedTaskA, true)

	keep := "refs/multica/tasks/" + taskKey(sharedTaskA) + "/base"
	if got := gitRefCommit(t, f.barePath, keep); got != f.baseCommit {
		t.Fatalf("%s = %s, want base %s", keep, got, f.baseCommit)
	}

	dropCacheRefsAndGC(t, f.barePath)
	if !hasObject(res.Path, f.baseCommit) {
		t.Fatal("cache gc pruned an object the task borrows")
	}
	gitIn(t, res.Path, "fsck", "--connectivity-only")
}

func TestSharedObjectsWithoutKeepRefLosesBaseToCacheGC(t *testing.T) {
	t.Parallel()
	// Control: proves the GC helper really prunes, so the pin test above can fail.
	f := newSharedFixture(t)
	res := f.checkout(t, sharedTaskA, true)
	gitIn(t, f.barePath, "update-ref", "-d", "refs/multica/tasks/"+taskKey(sharedTaskA)+"/base")
	dropCacheRefsAndGC(t, f.barePath)
	if hasObject(res.Path, f.baseCommit) {
		t.Fatal("control: expected the unpinned base to be pruned")
	}
}

func TestSharedObjectsReuseAddsKeepRefForEveryTask(t *testing.T) {
	t.Parallel()
	f := newSharedFixture(t)
	f.checkout(t, sharedTaskA, true)
	// A later task in the same workdir pins its own base even when the caller
	// no longer sends the hint: the checkout already borrows from the cache.
	res := f.checkout(t, sharedTaskB, false)

	keys, err := KeepRefTaskKeys(context.Background(), f.barePath)
	if err != nil {
		t.Fatalf("KeepRefTaskKeys: %v", err)
	}
	want := []string{taskKey(sharedTaskB), taskKey(sharedTaskA)}
	slices.Sort(want)
	if !slices.Equal(keys, want) {
		t.Fatalf("keep-ref task keys = %v, want %v", keys, want)
	}
	if got := CheckoutKeepRefTaskKeys(res.Path); !slices.Equal(got, want) {
		t.Fatalf("checkout keep-ref record = %v, want %v", got, want)
	}
}

func TestSharedObjectsMigratesLinkedWorktreeAndCarriesUnpushedWork(t *testing.T) {
	t.Parallel()
	f := newSharedFixture(t)
	linked := f.checkout(t, sharedTaskA, false)
	if !isGitWorktree(linked.Path) {
		t.Fatal("setup: expected a linked worktree")
	}
	gitIn(t, linked.Path, "commit", "--allow-empty", "-m", "unpushed")
	unpushed := gitHead(t, linked.Path)

	res, err := f.cache.CreateWorktree(WorktreeParams{
		WorkspaceID: "ws-1", RepoURL: f.source, WorkDir: f.workDir,
		AgentName: "Sandboxed", TaskID: sharedTaskB, SharedObjects: true, Fresh: true,
	})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if res.Path != linked.Path || isGitWorktree(res.Path) && !isIsolatedCheckout(res.Path) {
		t.Fatalf("checkout was not migrated to standalone metadata: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(res.Path, ".git", "objects", "info", "alternates")); err != nil {
		t.Fatalf("migrated checkout has no alternates: %v", err)
	}
	out := gitIn(t, f.barePath, "worktree", "list", "--porcelain")
	if strings.Contains(out, "worktree "+linked.Path+"\n") {
		t.Fatalf("cache kept the migrated worktree entry:\n%s", out)
	}
	if !hasObject(res.Path, unpushed) {
		t.Fatal("unpushed commit was lost in migration")
	}

	// The cache drops the old task branch and prunes; the carried commit must survive.
	dropCacheRefsAndGC(t, f.barePath)
	if !hasObject(res.Path, unpushed) {
		t.Fatal("cache gc pruned the carried unpushed commit")
	}
	gitIn(t, res.Path, "fsck", "--connectivity-only")
}

func TestPruneKeepRefsReleasesOnlyDeadTasks(t *testing.T) {
	t.Parallel()
	f := newSharedFixture(t)
	f.checkout(t, sharedTaskA, true)
	f.checkout(t, sharedTaskB, true)

	live := map[string]struct{}{taskKey(sharedTaskA): {}}
	released, remaining, err := PruneKeepRefs(context.Background(), f.barePath, live)
	if err != nil {
		t.Fatalf("PruneKeepRefs: %v", err)
	}
	if released != 1 || remaining != 1 {
		t.Fatalf("released=%d remaining=%d, want 1 and 1", released, remaining)
	}
	keys, _ := KeepRefTaskKeys(context.Background(), f.barePath)
	if !slices.Equal(keys, []string{taskKey(sharedTaskA)}) {
		t.Fatalf("keys after prune = %v", keys)
	}

	released, remaining, err = PruneKeepRefs(context.Background(), f.barePath, nil)
	if err != nil || released != 1 || remaining != 0 {
		t.Fatalf("second prune released=%d remaining=%d err=%v, want 1, 0, nil", released, remaining, err)
	}
}
