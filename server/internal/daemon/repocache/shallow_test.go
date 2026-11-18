package repocache

import (
	"path/filepath"
	"strings"
	"testing"
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
// (CHE-7286). gitFetch must heal that, or later checkouts and reviews fail
// with missing history objects.
func TestGitFetchUnshallowsCacheShallowedFromWorktree(t *testing.T) {
	source := createFilterableTestRepo(t)
	cache := New(t.TempDir(), nil)
	barePath := seedBloblessCache(t, cache, "ws", source)

	wt := filepath.Join(t.TempDir(), "wt")
	if out, err := runGitCombinedOutput("-C", barePath, "worktree", "add", "--detach", wt, "HEAD"); err != nil {
		t.Fatalf("worktree add: %s: %v", out, err)
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
