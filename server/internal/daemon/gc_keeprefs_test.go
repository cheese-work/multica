package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	liveKeepKey = "aaaaaaaaaaaa"
	deadKeepKey = "bbbbbbbbbbbb"
)

// pinTestKeepRef points a task keep-ref at the repo's HEAD, as a shared-objects
// checkout does.
func pinTestKeepRef(t *testing.T, barePath, key string) string {
	t.Helper()
	ref := "refs/multica/tasks/" + key + "/base"
	runGitForGC(t, "", "-C", barePath, "update-ref", ref, "HEAD")
	return ref
}

// recordTestCheckout creates a task checkout that records keys as its borrowed
// keep-refs, at the path a real one has under the workspaces root.
func recordTestCheckout(t *testing.T, d *Daemon, keys ...string) string {
	t.Helper()
	return recordTestCheckoutAt(t, d, filepath.Join("workdir", "repo"), keys...)
}

// recordTestCheckoutAt is recordTestCheckout at a path below the task env
// directory, for checkouts the repo checkout API puts deeper than the workdir.
func recordTestCheckoutAt(t *testing.T, d *Daemon, rel string, keys ...string) string {
	t.Helper()
	checkout := filepath.Join(d.cfg.WorkspacesRoot, "ws", "task-env", rel)
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(checkout, ".git", "multica-keep-refs")
	if err := os.WriteFile(record, []byte(strings.Join(keys, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return checkout
}

func TestMaintainRepoCacheReleasesOnlyOrphanKeepRefs(t *testing.T) {
	t.Parallel()

	d := newGCTestDaemon(t, http.NewServeMux())
	barePath := newEvictTestRepo(t, d, "11111111-1111-1111-1111-111111111111", testRepoURL)
	live := pinTestKeepRef(t, barePath, liveKeepKey)
	dead := pinTestKeepRef(t, barePath, deadKeepKey)
	recordTestCheckout(t, d, liveKeepKey)

	d.maintainRepoCache(t.Context(), barePath, &gcStats{})

	if !gitRefExists(t, barePath, live) {
		t.Errorf("keep-ref %s of a live checkout was released", live)
	}
	if gitRefExists(t, barePath, dead) {
		t.Errorf("keep-ref %s has no borrowing checkout but was kept", dead)
	}
}

func TestEvictRepoCacheKeepsRepoWhileKeepRefBorrowerLives(t *testing.T) {
	t.Parallel()

	d := newGCTestDaemon(t, http.NewServeMux())
	d.cfg.GCRepoTTL = 24 * time.Hour
	wsID := "11111111-1111-1111-1111-111111111111"
	barePath := newEvictTestRepo(t, d, wsID, testRepoURL)
	writeLastUsed(t, barePath, time.Now().Add(-90*24*time.Hour))
	pinTestKeepRef(t, barePath, liveKeepKey)
	checkout := recordTestCheckout(t, d, liveKeepKey)

	if stats := runRepoGC(d); stats.repoCachesReclaimed != 0 {
		t.Fatalf("repo_caches_reclaimed = %d, want 0 while a checkout borrows the cache", stats.repoCachesReclaimed)
	}
	if _, err := os.Stat(barePath); err != nil {
		t.Fatalf("cache a checkout borrows from was evicted: %v", err)
	}

	// Once the borrower is gone the pin is released and the idle cache goes.
	if err := os.RemoveAll(checkout); err != nil {
		t.Fatal(err)
	}
	if stats := runRepoGC(d); stats.repoCachesReclaimed != 1 {
		t.Fatalf("repo_caches_reclaimed = %d, want 1 after the borrower is gone", stats.repoCachesReclaimed)
	}
}

func TestRepoCheckoutForwardsSharedMode(t *testing.T) {
	t.Parallel()

	const workspaceID = "ws-checkout"
	const repoURL = "https://github.com/org/repo.git"
	cache := &recordingRepoCache{lookupPath: "/cache/org/repo.git"}
	workDir := t.TempDir()
	d := newRepoCheckoutTestDaemon(t, workspaceID, repoURL, workDir, cache)

	rec := httptest.NewRecorder()
	body := strings.NewReader(`{"url":"` + repoURL + `","workspace_id":"` + workspaceID + `","workdir":"` + workDir + `","task_id":"task-1","checkout_mode":"shared"}`)
	d.repoCheckoutHandler().ServeHTTP(rec, authorizedRepoCheckoutRequest(body))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !cache.lastCreateParams().SharedObjects {
		t.Fatal("shared checkout_mode was not forwarded to repo cache")
	}
}

func TestMaintainRepoCacheKeepsPinsOfNestedCheckout(t *testing.T) {
	t.Parallel()

	d := newGCTestDaemon(t, http.NewServeMux())
	barePath := newEvictTestRepo(t, d, "11111111-1111-1111-1111-111111111111", testRepoURL)
	live := pinTestKeepRef(t, barePath, liveKeepKey)
	recordTestCheckoutAt(t, d, filepath.Join("workdir", "one", "two", "repo"), liveKeepKey)

	d.maintainRepoCache(t.Context(), barePath, &gcStats{})

	if !gitRefExists(t, barePath, live) {
		t.Errorf("keep-ref %s of a checkout three directories below the workdir was released", live)
	}
}

func TestEvictRepoCacheKeepsRepoWhileNestedBorrowerLives(t *testing.T) {
	t.Parallel()

	d := newGCTestDaemon(t, http.NewServeMux())
	d.cfg.GCRepoTTL = 24 * time.Hour
	barePath := newEvictTestRepo(t, d, "11111111-1111-1111-1111-111111111111", testRepoURL)
	writeLastUsed(t, barePath, time.Now().Add(-90*24*time.Hour))
	pinTestKeepRef(t, barePath, liveKeepKey)
	recordTestCheckoutAt(t, d, filepath.Join("workdir", "one", "two", "repo"), liveKeepKey)

	if stats := runRepoGC(d); stats.repoCachesReclaimed != 0 {
		t.Fatalf("repo_caches_reclaimed = %d, want 0 while a nested checkout borrows the cache", stats.repoCachesReclaimed)
	}
	if _, err := os.Stat(barePath); err != nil {
		t.Fatalf("cache a nested checkout borrows from was evicted: %v", err)
	}
}

func TestMaintainRepoCacheKeepsPinsWhenBorrowerRecordUnreadable(t *testing.T) {
	t.Parallel()

	d := newGCTestDaemon(t, http.NewServeMux())
	d.cfg.GCRepoTTL = 24 * time.Hour
	barePath := newEvictTestRepo(t, d, "11111111-1111-1111-1111-111111111111", testRepoURL)
	writeLastUsed(t, barePath, time.Now().Add(-90*24*time.Hour))
	live := pinTestKeepRef(t, barePath, liveKeepKey)
	checkout := recordTestCheckout(t, d, liveKeepKey)
	// A directory where the record belongs makes the read fail.
	record := filepath.Join(checkout, ".git", "multica-keep-refs")
	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(record, 0o755); err != nil {
		t.Fatal(err)
	}

	if stats := runRepoGC(d); stats.repoCachesReclaimed != 0 {
		t.Fatalf("repo_caches_reclaimed = %d, want 0 while a borrower record is unreadable", stats.repoCachesReclaimed)
	}
	if !gitRefExists(t, barePath, live) {
		t.Errorf("keep-ref %s was released although its borrower's record could not be read", live)
	}
}

func TestMaintainRepoCacheKeepsPinsOfCheckoutBelowOrdinaryCheckout(t *testing.T) {
	t.Parallel()

	d := newGCTestDaemon(t, http.NewServeMux())
	barePath := newEvictTestRepo(t, d, "11111111-1111-1111-1111-111111111111", testRepoURL)
	live := pinTestKeepRef(t, barePath, liveKeepKey)
	// An ordinary isolated checkout has a standalone .git and records nothing.
	outer := recordTestCheckoutAt(t, d, filepath.Join("workdir", "outer"))
	if err := os.Remove(filepath.Join(outer, ".git", "multica-keep-refs")); err != nil {
		t.Fatal(err)
	}
	recordTestCheckoutAt(t, d, filepath.Join("workdir", "outer", "vendor", "repo"), liveKeepKey)

	d.maintainRepoCache(t.Context(), barePath, &gcStats{})

	if !gitRefExists(t, barePath, live) {
		t.Errorf("keep-ref %s of a checkout below an ordinary checkout was released", live)
	}
}

func TestMaintainRepoCacheKeepsPinsWhenTaskDirectoryUnreadable(t *testing.T) {
	t.Parallel()

	d := newGCTestDaemon(t, http.NewServeMux())
	barePath := newEvictTestRepo(t, d, "11111111-1111-1111-1111-111111111111", testRepoURL)
	live := pinTestKeepRef(t, barePath, liveKeepKey)
	checkout := recordTestCheckout(t, d, liveKeepKey)
	taskEnv := filepath.Dir(filepath.Dir(checkout))
	if err := os.Chmod(taskEnv, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(taskEnv, 0o755) })
	if _, err := os.Lstat(filepath.Join(taskEnv, "workdir")); err == nil {
		t.Skip("directory permissions are not enforced for this user")
	}

	d.maintainRepoCache(t.Context(), barePath, &gcStats{})

	if !gitRefExists(t, barePath, live) {
		t.Errorf("keep-ref %s was released although its task directory could not be listed", live)
	}
}

func TestMaintainRepoCacheReleasesOrphanPinsAmongLinkedWorktrees(t *testing.T) {
	t.Parallel()

	d := newGCTestDaemon(t, http.NewServeMux())
	barePath := newEvictTestRepo(t, d, "11111111-1111-1111-1111-111111111111", testRepoURL)
	live := pinTestKeepRef(t, barePath, liveKeepKey)
	dead := pinTestKeepRef(t, barePath, deadKeepKey)
	recordTestCheckout(t, d, liveKeepKey)
	// A linked worktree's .git is a file, not a directory.
	linked := filepath.Join(d.cfg.WorkspacesRoot, "ws", "task-env", "workdir", "linked")
	if err := os.MkdirAll(linked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: /cache/worktrees/linked\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	d.maintainRepoCache(t.Context(), barePath, &gcStats{})

	if !gitRefExists(t, barePath, live) {
		t.Errorf("keep-ref %s of a live checkout was released", live)
	}
	if gitRefExists(t, barePath, dead) {
		t.Errorf("orphan keep-ref %s was kept because a linked worktree sits beside the live checkout", dead)
	}
}

// cancelAfterCtx reports cancellation from its n+1th Err call on and counts
// the calls, so a test can cancel a scan part way through it.
type cancelAfterCtx struct {
	context.Context
	after int
	calls int
}

func (c *cancelAfterCtx) Err() error {
	c.calls++
	if c.calls > c.after {
		return context.Canceled
	}
	return nil
}

func TestLiveKeepRefTaskKeysStopsWhenMaintenanceIsCancelled(t *testing.T) {
	t.Parallel()

	d := newGCTestDaemon(t, http.NewServeMux())
	recordTestCheckout(t, d, liveKeepKey)
	workdir := filepath.Join(d.cfg.WorkspacesRoot, "ws", "task-env", "workdir")
	const dirs = 200
	for i := range dirs {
		if err := os.MkdirAll(filepath.Join(workdir, "bulk", strings.Repeat("d", 1+i%7), string(rune('a'+i%26)), "x"+strings.Repeat("y", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	ctx := &cancelAfterCtx{Context: t.Context(), after: 10}
	if _, err := liveKeepRefTaskKeys(ctx, d.cfg.WorkspacesRoot); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if ctx.calls > 20 {
		t.Fatalf("scan checked for cancellation %d times, want it to stop right after the 10th", ctx.calls)
	}
}

func TestMaintainRepoCacheKeepsPinsWhenMaintenanceIsCancelled(t *testing.T) {
	t.Parallel()

	d := newGCTestDaemon(t, http.NewServeMux())
	barePath := newEvictTestRepo(t, d, "11111111-1111-1111-1111-111111111111", testRepoURL)
	dead := pinTestKeepRef(t, barePath, deadKeepKey)
	recordTestCheckout(t, d, liveKeepKey)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	d.releaseOrphanKeepRefsLocked(ctx, barePath)

	if !gitRefExists(t, barePath, dead) {
		t.Errorf("keep-ref %s was released by a cancelled maintenance pass", dead)
	}
}
