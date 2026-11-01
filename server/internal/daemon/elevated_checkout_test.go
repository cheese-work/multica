package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/repocache"
)

// Elevated-run checkout (CHE-2216 plan 3.5, slice B7a): an elevated task gets
// the synthetic base of its grant's pinned commit, never a worktree.

const elevatedUpstreamMsg = "UPSTREAM-PRIVATE-MESSAGE do not leak"

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=u", "GIT_AUTHOR_EMAIL=u@x", "GIT_COMMITTER_NAME=u", "GIT_COMMITTER_EMAIL=u@x")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// bareWithTwoCommits returns a bare cache and its two commit SHAs.
func bareWithTwoCommits(t *testing.T) (cache, c1, c2 string) {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "w")
	cache = filepath.Join(root, "cache.git")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "init", "-b", "main")
	for i, name := range []string{"one.txt", "two.txt"} {
		if err := os.WriteFile(filepath.Join(work, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, work, "add", "-A")
		runGit(t, work, "commit", "-m", elevatedUpstreamMsg+" "+name)
		if i == 0 {
			c1 = runGit(t, work, "rev-parse", "HEAD")
		}
	}
	c2 = runGit(t, work, "rev-parse", "HEAD")
	runGit(t, root, "clone", "--bare", work, cache)
	return cache, c1, c2
}

func checkoutRequest(t *testing.T, d *Daemon, ws, repoURL, workDir string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	body := strings.NewReader(`{"url":"` + repoURL + `","workspace_id":"` + ws + `","workdir":"` + workDir + `","task_id":"task-1"}`)
	d.repoCheckoutHandler().ServeHTTP(rec, authorizedRepoCheckoutRequest(body))
	return rec
}

func elevatedTask(workDir, base, head string) activeRepoCheckoutTask {
	return activeRepoCheckoutTask{
		WorkspaceID: "ws-checkout", TaskID: "task-1", AgentID: "agent-1", AgentName: "Test Agent", WorkDir: workDir,
		IssueIdentifier: "CHE-1", Elevated: true, PinnedBaseSHA: base, PinnedHeadSHA: head,
	}
}

func TestElevatedRepoCheckoutBuildsTheSyntheticBaseOfTheHeadPin(t *testing.T) {
	t.Parallel()
	const ws, repoURL = "ws-checkout", "https://github.com/org/repo.git"
	cachePath, c1, c2 := bareWithTwoCommits(t)
	cache := &recordingRepoCache{lookupPath: cachePath}
	workDir := t.TempDir()
	d := newRepoCheckoutTestDaemon(t, ws, repoURL, workDir, cache)
	d.registerActiveRepoCheckoutTask("mat_repo_checkout_test", elevatedTask(workDir, c1, c2))

	rec := checkoutRequest(t, d, ws, repoURL, workDir)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var res repocache.WorktreeResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(workDir, "repo"); res.Path != want {
		t.Fatalf("path %q, want %q", res.Path, want)
	}
	if got := cache.lastCreateParams(); got.WorkDir != "" {
		t.Fatalf("an elevated checkout reached CreateWorktree: %+v", got)
	}
	// The head pin (c2) won over the base pin (c1): both files, one root commit.
	for _, f := range []string{"one.txt", "two.txt"} {
		if _, err := os.Stat(filepath.Join(res.Path, f)); err != nil {
			t.Fatalf("pinned tree misses %s: %v", f, err)
		}
	}
	if n := strings.Count(runGit(t, res.Path, "rev-list", "--all"), "\n") + 1; n != 1 {
		t.Fatalf("repository holds %d commits, want only the synthetic root", n)
	}
	if out := runGit(t, res.Path, "log", "--all", "--format=%B"); strings.Contains(out, elevatedUpstreamMsg) {
		t.Fatal("upstream commit message reached the elevated checkout")
	}
	if got := runGit(t, res.Path, "rev-parse", "--abbrev-ref", "HEAD"); got != res.BranchName || !strings.HasPrefix(got, "agent/") {
		t.Fatalf("branch %q, result %q", got, res.BranchName)
	}
}

func TestElevatedRepoCheckoutWithoutAPinFailsClosed(t *testing.T) {
	t.Parallel()
	const ws, repoURL = "ws-checkout", "https://github.com/org/repo.git"
	cachePath, _, _ := bareWithTwoCommits(t)
	cache := &recordingRepoCache{lookupPath: cachePath}
	workDir := t.TempDir()
	d := newRepoCheckoutTestDaemon(t, ws, repoURL, workDir, cache)
	d.registerActiveRepoCheckoutTask("mat_repo_checkout_test", elevatedTask(workDir, "", ""))

	rec := checkoutRequest(t, d, ws, repoURL, workDir)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := cache.lastCreateParams(); got.WorkDir != "" {
		t.Fatalf("an unpinned elevated checkout reached CreateWorktree: %+v", got)
	}
	if entries, _ := os.ReadDir(workDir); len(entries) != 0 {
		t.Fatalf("refusal left %d entries in the workdir", len(entries))
	}
}

func TestTaskDecodesPinnedSHAs(t *testing.T) {
	var task Task
	if err := json.Unmarshal([]byte(`{"id":"t","profile":"elevated","pinned_base_sha":"b","pinned_head_sha":"h"}`), &task); err != nil {
		t.Fatal(err)
	}
	if task.PinnedBaseSHA != "b" || task.PinnedHeadSHA != "h" {
		t.Fatalf("got %+v", task)
	}
}

// The claim's profile and pins reach the checkout registration; a claim from a
// server that predates the profile stays restricted.
func TestNewActiveRepoCheckoutTaskCarriesTheClaimPins(t *testing.T) {
	var elevated, legacy Task
	if err := json.Unmarshal([]byte(`{"id":"t","agent":{"name":"a"},"profile":"elevated","pinned_base_sha":"b","pinned_head_sha":"h"}`), &elevated); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"id":"t","agent":{"name":"a"},"pinned_base_sha":"b"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if got := newActiveRepoCheckoutTask(elevated, "/w"); !got.Elevated || got.PinnedBaseSHA != "b" || got.PinnedHeadSHA != "h" {
		t.Fatalf("elevated claim registered as %+v", got)
	}
	if got := newActiveRepoCheckoutTask(legacy, "/w"); got.Elevated {
		t.Fatalf("claim without a profile registered as elevated: %+v", got)
	}
}

func TestElevatedRepoCheckoutRepeatReusesTheCheckoutAndKeepsLocalWork(t *testing.T) {
	t.Parallel()
	const ws, repoURL = "ws-checkout", "https://github.com/org/repo.git"
	cachePath, c1, c2 := bareWithTwoCommits(t)
	cache := &recordingRepoCache{lookupPath: cachePath}
	workDir := t.TempDir()
	d := newRepoCheckoutTestDaemon(t, ws, repoURL, workDir, cache)
	d.registerActiveRepoCheckoutTask("mat_repo_checkout_test", elevatedTask(workDir, c1, c2))

	first := checkoutRequest(t, d, ws, repoURL, workDir)
	if first.Code != http.StatusOK {
		t.Fatalf("first: expected 200, got %d: %s", first.Code, first.Body.String())
	}
	var res repocache.WorktreeResult
	if err := json.Unmarshal(first.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(res.Path, "local.txt")
	if err := os.WriteFile(local, []byte("agent work\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	second := checkoutRequest(t, d, ws, repoURL, workDir)
	if second.Code != http.StatusOK {
		t.Fatalf("repeat: expected 200, got %d: %s", second.Code, second.Body.String())
	}
	if second.Body.String() != first.Body.String() {
		t.Fatalf("repeat answered %s, first answered %s", second.Body.String(), first.Body.String())
	}
	if b, err := os.ReadFile(local); err != nil || string(b) != "agent work\n" {
		t.Fatalf("repeat checkout lost local work: %q, %v", b, err)
	}
}

func TestElevatedRepoCheckoutRefusesANonEmptyDestinationItDidNotBuild(t *testing.T) {
	t.Parallel()
	const ws, repoURL = "ws-checkout", "https://github.com/org/repo.git"
	cachePath, c1, c2 := bareWithTwoCommits(t)
	cache := &recordingRepoCache{lookupPath: cachePath}
	workDir := t.TempDir()
	d := newRepoCheckoutTestDaemon(t, ws, repoURL, workDir, cache)
	d.registerActiveRepoCheckoutTask("mat_repo_checkout_test", elevatedTask(workDir, c1, c2))
	foreign := filepath.Join(workDir, "repo")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreign, "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := checkoutRequest(t, d, ws, repoURL, workDir)
	if rec.Code == http.StatusOK {
		t.Fatalf("a foreign non-empty destination was accepted: %s", rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(foreign, "x")); err != nil {
		t.Fatalf("refusal touched the foreign file: %v", err)
	}
}
