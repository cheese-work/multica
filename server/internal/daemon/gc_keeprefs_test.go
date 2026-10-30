package daemon

import (
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
	checkout := filepath.Join(d.cfg.WorkspacesRoot, "ws", "task-env", "workdir", "repo")
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
