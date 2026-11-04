package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Intake-run pinning (CHE-2216 plan 3.6, slice B6; T11 intake part).

func TestPinAllowsOnlyThePRHeadRef(t *testing.T) {
	t.Parallel()
	const pin = "feature/pr-head"
	cases := map[string]bool{
		"refs/heads/feature/pr-head":          true,
		"refs/heads/feature/pr-head2":         false,
		"refs/heads/main":                     false,
		"refs/pull/5/head":                    false, // fork PR ref
		"refs/remotes/fork/feature/pr-head":   false,
		"refs/remotes/origin/feature/pr-head": false,
		"fork:feature/pr-head":                false,
		"feature/pr-head":                     false, // short names are not refs
		"refs/tags/feature/pr-head":           false,
		"":                                    false,
	}
	for ref, want := range cases {
		if got := pinAllowsRef(pin, ref); got != want {
			t.Errorf("pinAllowsRef(%q, %q) = %v, want %v", pin, ref, got, want)
		}
	}
	// An unresolved pin (fork PR, no head ref) allows nothing.
	for _, ref := range []string{"refs/heads/feature/pr-head", "refs/heads/", "refs/heads/main", ""} {
		if pinAllowsRef("", ref) {
			t.Errorf("empty pin allowed %q", ref)
		}
	}
	// A pin that is not a plain branch name allows nothing.
	for _, bad := range []string{"-x", "a..b", "a b", "a:b", "refs/heads/x", "x/", "/x", "x.lock", "a\x00b", "a~b", "a^b", "a\\b"} {
		if pinAllowsRef(bad, "refs/heads/"+bad) {
			t.Errorf("malformed pin %q allowed", bad)
		}
	}
}

func TestRepoCheckoutOfPinnedIntakeRunIgnoresRequestedRef(t *testing.T) {
	t.Parallel()
	const workspaceID = "ws-checkout"
	const repoURL = "https://github.com/org/repo.git"
	cache := &recordingRepoCache{lookupPath: "/cache/org/repo.git"}
	workDir := t.TempDir()
	d := newRepoCheckoutTestDaemon(t, workspaceID, repoURL, workDir, cache)
	d.registerActiveRepoCheckoutTask("mat_repo_checkout_test", activeRepoCheckoutTask{
		WorkspaceID: workspaceID, TaskID: "task-1", AgentID: "agent-1", AgentName: "Test Agent", WorkDir: workDir,
		IntakeRun: true, PinnedBranch: "feature/pr-head", PinnedRepo: "github.com/org/repo",
	})

	rec := httptest.NewRecorder()
	body := strings.NewReader(`{"url":"` + repoURL + `","workspace_id":"` + workspaceID + `","workdir":"` + workDir + `","task_id":"task-1","ref":"someone-else:main"}`)
	d.repoCheckoutHandler().ServeHTTP(rec, authorizedRepoCheckoutRequest(body))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	got := cache.lastCreateParams()
	if got.Ref != "feature/pr-head" || got.Branch != "feature/pr-head" {
		t.Fatalf("CreateWorktree Ref=%q Branch=%q, want both feature/pr-head", got.Ref, got.Branch)
	}
}

// A run for repo A must not get repo B's checkout just because B is configured
// and has a branch of the same name.
func TestRepoCheckoutOfPinnedIntakeRunRefusesAnotherRepository(t *testing.T) {
	t.Parallel()
	const workspaceID = "ws-checkout"
	const prRepo = "https://github.com/org/repo.git"
	const otherRepo = "https://github.com/org/other.git"
	cache := &recordingRepoCache{lookupPath: "/cache/org/repo.git"}
	workDir := t.TempDir()
	d := newRepoCheckoutTestDaemonWithRepos(t, workspaceID, []string{prRepo, otherRepo}, workDir, cache)
	d.registerActiveRepoCheckoutTask("mat_repo_checkout_test", activeRepoCheckoutTask{
		WorkspaceID: workspaceID, TaskID: "task-1", AgentID: "agent-1", AgentName: "Test Agent", WorkDir: workDir,
		IntakeRun: true, PinnedBranch: "feature/pr-head", PinnedRepo: "github.com/org/repo",
	})

	for url, want := range map[string]int{prRepo: http.StatusOK, otherRepo: http.StatusForbidden} {
		cache.params = nil
		rec := httptest.NewRecorder()
		body := strings.NewReader(`{"url":"` + url + `","workspace_id":"` + workspaceID + `","workdir":"` + workDir + `","task_id":"task-1"}`)
		d.repoCheckoutHandler().ServeHTTP(rec, authorizedRepoCheckoutRequest(body))
		if rec.Code != want {
			t.Fatalf("checkout of %s: got %d, want %d: %s", url, rec.Code, want, rec.Body.String())
		}
		if want == http.StatusForbidden && len(cache.params) != 0 {
			t.Fatalf("a refused checkout of %s reached the cache: %+v", url, cache.params)
		}
	}
}

func TestPinAllowsOnlyThePRRepository(t *testing.T) {
	t.Parallel()
	const pin = "github.com/org/repo"
	for url, want := range map[string]bool{
		"https://github.com/org/repo.git":   true,
		"https://github.com/Org/Repo":       true,
		"git@github.com:org/repo.git":       true,
		"https://github.com/org/other.git":  false,
		"https://gitlab.com/org/repo.git":   false,
		"https://github.com/other-org/repo": false,
		"":                                  false,
	} {
		if got := pinAllowsRepo(pin, url); got != want {
			t.Errorf("pinAllowsRepo(%q, %q) = %v, want %v", pin, url, got, want)
		}
	}
	if pinAllowsRepo("", "https://github.com/org/repo.git") {
		t.Error("empty pin allowed a repository")
	}
}

func TestRepoCheckoutOfUnresolvedIntakeRunFailsClosed(t *testing.T) {
	t.Parallel()
	const workspaceID = "ws-checkout"
	const repoURL = "https://github.com/org/repo.git"
	cache := &recordingRepoCache{lookupPath: "/cache/org/repo.git"}
	workDir := t.TempDir()
	d := newRepoCheckoutTestDaemon(t, workspaceID, repoURL, workDir, cache)
	d.registerActiveRepoCheckoutTask("mat_repo_checkout_test", activeRepoCheckoutTask{
		WorkspaceID: workspaceID, TaskID: "task-1", AgentID: "agent-1", AgentName: "Test Agent", WorkDir: workDir,
		IntakeRun: true, // fork PR or missing head ref: no pin resolved
	})

	rec := httptest.NewRecorder()
	body := strings.NewReader(`{"url":"` + repoURL + `","workspace_id":"` + workspaceID + `","workdir":"` + workDir + `","task_id":"task-1"}`)
	d.repoCheckoutHandler().ServeHTTP(rec, authorizedRepoCheckoutRequest(body))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}

	// A branch without its repository is just as unresolved.
	d.registerActiveRepoCheckoutTask("mat_repo_checkout_test", activeRepoCheckoutTask{
		WorkspaceID: workspaceID, TaskID: "task-1", AgentID: "agent-1", AgentName: "Test Agent", WorkDir: workDir,
		IntakeRun: true, PinnedBranch: "feature/pr-head",
	})
	rec = httptest.NewRecorder()
	body = strings.NewReader(`{"url":"` + repoURL + `","workspace_id":"` + workspaceID + `","workdir":"` + workDir + `","task_id":"task-1"}`)
	d.repoCheckoutHandler().ServeHTTP(rec, authorizedRepoCheckoutRequest(body))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("branch without repository: expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestTaskDecodesIntakePin(t *testing.T) {
	t.Parallel()
	var task Task
	if err := json.Unmarshal([]byte(`{"id":"t","intake_run":true,"pinned_branch":"feature/pr-head","pinned_repo":"github.com/org/repo"}`), &task); err != nil {
		t.Fatal(err)
	}
	if !task.IntakeRun || task.PinnedBranch != "feature/pr-head" || task.PinnedRepo != "github.com/org/repo" {
		t.Fatalf("task = %+v", task)
	}
}
