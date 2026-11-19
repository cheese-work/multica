package repocache

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func isShallow(t *testing.T, repo string) bool {
	t.Helper()
	out, err := runGitOutput("-C", repo, "rev-parse", "--is-shallow-repository")
	if err != nil {
		t.Fatalf("rev-parse --is-shallow-repository: %v", err)
	}
	return strings.TrimSpace(string(out)) == "true"
}

// A linked worktree shares its bare cache's `shallow` file, so one
// `git fetch --depth` run from any worktree turns the shared cache shallow
// (CHE-7286). gitFetch must heal a cache that is already shallow, or later
// checkouts and reviews fail with missing history objects.
func TestGitFetchUnshallowsCacheShallowedFromWorktree(t *testing.T) {
	source := createFilterableTestRepo(t)
	cache := New(t.TempDir(), nil)
	barePath := seedBloblessCache(t, cache, "ws", source)

	wt := filepath.Join(t.TempDir(), "wt")
	if out, err := runGitCombinedOutput("-C", barePath, "worktree", "add", "--detach", wt, "HEAD"); err != nil {
		t.Fatalf("worktree add: %s: %v", out, err)
	}
	// A cache from before the guard existed has no `shallow` directory.
	if err := os.RemoveAll(filepath.Join(barePath, "shallow")); err != nil {
		t.Fatalf("remove guard: %v", err)
	}
	if out, err := runGitCombinedOutput("-C", wt, "fetch", "--depth=1", "origin", "HEAD"); err != nil {
		t.Fatalf("shallow fetch from worktree: %s: %v", out, err)
	}
	if !isShallow(t, barePath) {
		t.Fatal("precondition: shallow fetch from a worktree should shallow the shared cache")
	}

	if err := gitFetch(barePath); err != nil {
		t.Fatalf("gitFetch: %v", err)
	}
	if isShallow(t, barePath) {
		t.Fatal("cache is still shallow after gitFetch")
	}
}

// Prevention, not just healing: after the cache has been fetched once, a
// shallow fetch run by plain `git` from a linked worktree or straight in the
// bare cache — no guard shim involved — must leave the cache complete at once,
// not only after the next daemon fetch (CHE-7318).
func TestShallowFetchCannotShallowGuardedCache(t *testing.T) {
	source := createFilterableTestRepo(t)
	cache := New(t.TempDir(), nil)
	barePath := seedBloblessCache(t, cache, "ws", source)
	if err := gitFetch(barePath); err != nil {
		t.Fatalf("gitFetch: %v", err)
	}

	wt := filepath.Join(t.TempDir(), "wt")
	if out, err := runGitCombinedOutput("-C", barePath, "worktree", "add", "--detach", wt, "HEAD"); err != nil {
		t.Fatalf("worktree add: %s: %v", out, err)
	}
	for name, dir := range map[string]string{"linked worktree": wt, "bare cache": barePath} {
		// The command may fail; only the cache state matters.
		_, _ = runGitCombinedOutput("-C", dir, "fetch", "--depth=1", "origin", "HEAD")
		if isShallow(t, barePath) {
			t.Fatalf("cache shallow right after a shallow fetch from the %s", name)
		}
	}

	// The guard must not break what the daemon does next.
	if err := gitFetch(barePath); err != nil {
		t.Fatalf("gitFetch after shallow attempts: %v", err)
	}
	if isShallow(t, barePath) {
		t.Fatal("cache shallow after gitFetch")
	}
}

// An isolated checkout seeds from the cache while an external Git process,
// which takes no repo lock, may fetch shallow into it. The guard must stay in
// place for the whole seeding, and objects must still be hard-linked (local) or
// borrowed (shared): `git clone` would drop both for a source holding a
// `shallow` entry (CHE-7318).
func TestIsolatedSeedingKeepsShallowGuardInPlace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("local clones copy objects on Windows")
	}
	for _, shared := range []bool{false, true} {
		name := map[bool]string{false: "local", true: "shared"}[shared]
		t.Run(name, func(t *testing.T) {
			source := createFilterableTestRepo(t)
			cache := New(t.TempDir(), nil)
			barePath := seedBloblessCache(t, cache, "ws", source)
			if err := gitFetch(barePath); err != nil {
				t.Fatalf("gitFetch: %v", err)
			}
			base, err := resolveCommit(barePath, "HEAD")
			if err != nil {
				t.Fatalf("resolve HEAD: %v", err)
			}

			guard := filepath.Join(barePath, "shallow")
			stop := make(chan struct{})
			done := make(chan struct{})
			var missing atomic.Bool
			go func() {
				defer close(done)
				for {
					select {
					case <-stop:
						return
					default:
					}
					if fi, err := os.Stat(guard); err != nil || !fi.IsDir() {
						missing.Store(true)
					}
					time.Sleep(time.Millisecond)
				}
			}()

			dest := filepath.Join(t.TempDir(), "co")
			_, err = createIsolatedCheckoutContext(context.Background(), barePath, source, dest, "agent/t", "HEAD", base, "", shared, false)
			close(stop)
			<-done
			if err != nil {
				t.Fatalf("createIsolatedCheckout: %v", err)
			}
			if missing.Load() {
				t.Fatal("shallow guard was absent while the isolated checkout was seeded")
			}
			// An unguarded shallow fetch right after must still fail.
			_, _ = runGitCombinedOutput("-C", barePath, "fetch", "--depth=1", "origin", "HEAD")
			if isShallow(t, barePath) {
				t.Fatal("cache shallow after seeding an isolated checkout")
			}

			if shared {
				if !hasAlternates(dest) {
					t.Fatal("shared checkout does not borrow the cache's objects")
				}
				return
			}
			linked := 0
			packs, _ := filepath.Glob(filepath.Join(barePath, "objects", "pack", "*.pack"))
			for _, pk := range packs {
				a, errA := os.Stat(pk)
				b, errB := os.Stat(filepath.Join(dest, ".git", "objects", "pack", filepath.Base(pk)))
				if errA == nil && errB == nil && os.SameFile(a, b) {
					linked++
				}
			}
			if len(packs) == 0 || linked == 0 {
				t.Fatalf("local checkout did not hardlink the cache's packs (%d packs, %d linked)", len(packs), linked)
			}
		})
	}
}
