package repocache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Pinned checkouts (WorktreeParams.Branch; CHE-2216 plan 3.6, slice B6) are the
// exact branch named, on the real cache, in both checkout shapes.

const pinnedBranch = "feature/pr-head"

func (f *existingCheckoutFixture) publishPinnedBranch(t *testing.T) string {
	t.Helper()
	runGitAuthored(t, f.source, "checkout", "-b", pinnedBranch)
	if err := os.WriteFile(filepath.Join(f.source, "pr.txt"), []byte("pr\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitAuthored(t, f.source, "add", "pr.txt")
	runGitAuthored(t, f.source, "commit", "-m", "pr head")
	head := gitHead(t, f.source)
	runGitAuthored(t, f.source, "checkout", f.defaultBranch)
	f.sync(t)
	return head
}

func (f *existingCheckoutFixture) sync(t *testing.T) {
	t.Helper()
	if err := f.cache.Sync("ws-1", []RepoInfo{{URL: f.source}}); err != nil {
		t.Fatalf("sync failed: %v", err)
	}
}

func (f *existingCheckoutFixture) pinned(workDir, branch string) (*WorktreeResult, error) {
	return f.cache.CreateWorktree(WorktreeParams{
		WorkspaceID:         "ws-1",
		RepoURL:             f.source,
		WorkDir:             workDir,
		Ref:                 branch,
		Branch:              branch,
		AgentName:           "Agent",
		TaskID:              firstTaskID,
		IsolatedGitMetadata: f.isolated,
		Fresh:               true, // a pinned checkout must ignore this
	})
}

func TestPinnedCheckoutIsTheExactBranch(t *testing.T) {
	for _, mode := range checkoutModes {
		t.Run(mode.name, func(t *testing.T) {
			f := newExistingCheckoutFixture(t, mode.isolated)
			head := f.publishPinnedBranch(t)

			result, err := f.pinned(f.workDir, pinnedBranch)
			if err != nil {
				t.Fatalf("pinned checkout: %v", err)
			}
			if result.BranchName != pinnedBranch || gitHead(t, result.Path) != head {
				t.Fatalf("branch=%q head=%s, want %q at %s", result.BranchName, gitHead(t, result.Path), pinnedBranch, head)
			}
			if got := currentBranchName(t, result.Path); got != pinnedBranch {
				t.Fatalf("checked-out branch = %q, want %q", got, pinnedBranch)
			}
		})
	}
}

// A local branch already carrying the pinned name must not push the run onto a
// timestamp-suffixed branch.
func TestPinnedCheckoutKeepsExactNameOnCollision(t *testing.T) {
	for _, mode := range checkoutModes {
		t.Run(mode.name, func(t *testing.T) {
			f := newExistingCheckoutFixture(t, mode.isolated)
			head := f.publishPinnedBranch(t)
			bare := f.cache.Lookup("ws-1", f.source)
			runGitAuthored(t, bare, "branch", pinnedBranch, f.defaultBranch) // behind origin/<B>

			result, err := f.pinned(f.workDir, pinnedBranch)
			if err != nil {
				t.Fatalf("pinned checkout: %v", err)
			}
			if result.BranchName != pinnedBranch || currentBranchName(t, result.Path) != pinnedBranch {
				t.Fatalf("branch = %q (checked out %q), want exactly %q", result.BranchName, currentBranchName(t, result.Path), pinnedBranch)
			}
			if got := gitHead(t, result.Path); got != head {
				t.Fatalf("head = %s, want the PR head %s", got, head)
			}
		})
	}
}

// Only the linked shape shares the cache's local branches; an isolated checkout
// clones the remote head fresh.
func TestPinnedCheckoutRefusesDivergedLocalBranch(t *testing.T) {
	f := newExistingCheckoutFixture(t, false)
	f.publishPinnedBranch(t)
	bare := f.cache.Lookup("ws-1", f.source)
	// A local commit on <B> that origin/<B> lacks.
	out, err := runGitOutput("-C", bare, "-c", "user.name=t", "-c", "user.email=t@t", "commit-tree", f.defaultBranch+"^{tree}", "-p", f.defaultBranch, "-m", "local only")
	if err != nil {
		t.Fatal(err)
	}
	local := strings.TrimSpace(string(out))
	runGitAuthored(t, bare, "branch", pinnedBranch, local)

	if _, err := f.pinned(f.workDir, pinnedBranch); err == nil {
		t.Fatal("pinned checkout moved a local branch that holds commits origin lacks")
	}
	got, _ := runGitOutput("-C", bare, "rev-parse", "refs/heads/"+pinnedBranch)
	if strings.TrimSpace(string(got)) != local {
		t.Fatal("refused checkout still moved the local branch")
	}
}

func TestPinnedCheckoutKeepsLocalWorkOnAnotherBranch(t *testing.T) {
	for _, mode := range checkoutModes {
		t.Run(mode.name, func(t *testing.T) {
			f := newExistingCheckoutFixture(t, mode.isolated)
			f.publishPinnedBranch(t)
			// An ordinary task checkout holding uncommitted work.
			ordinary := f.checkout(t, firstTaskID, false)
			scratch := filepath.Join(ordinary.Path, "scratch.txt")
			if err := os.WriteFile(scratch, []byte("mine\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			if _, err := f.pinned(f.workDir, pinnedBranch); err == nil {
				t.Fatal("pinned checkout replaced a checkout that holds local work")
			}
			if got := readFile(t, scratch); got != "mine\n" {
				t.Fatalf("local work lost: %q", got)
			}
			if got := currentBranchName(t, ordinary.Path); got != ordinary.BranchName {
				t.Fatalf("checkout moved to %q", got)
			}
		})
	}
}

func TestPinnedCheckoutMovesCleanCheckoutAndKeepsDirtyExactBranch(t *testing.T) {
	for _, mode := range checkoutModes {
		t.Run(mode.name, func(t *testing.T) {
			f := newExistingCheckoutFixture(t, mode.isolated)
			f.publishPinnedBranch(t)
			f.checkout(t, firstTaskID, false)

			result, err := f.pinned(f.workDir, pinnedBranch)
			if err != nil || result.BranchName != pinnedBranch {
				t.Fatalf("clean checkout: branch=%v err=%v, want %q", result, err, pinnedBranch)
			}
			scratch := filepath.Join(result.Path, "scratch.txt")
			if err := os.WriteFile(scratch, []byte("mine\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			again, err := f.pinned(f.workDir, pinnedBranch)
			if err != nil || again.BranchName != pinnedBranch {
				t.Fatalf("dirty exact branch: %v err=%v", again, err)
			}
			if got := readFile(t, scratch); got != "mine\n" {
				t.Fatalf("local work lost: %q", got)
			}
		})
	}
}

func TestPinnedCheckoutResolvesOnlyTheRemoteHead(t *testing.T) {
	for _, mode := range checkoutModes {
		t.Run(mode.name, func(t *testing.T) {
			f := newExistingCheckoutFixture(t, mode.isolated)
			runGitAuthored(t, f.source, "tag", "tag-only")
			f.sync(t)
			bare := f.cache.Lookup("ws-1", f.source)
			runGitAuthored(t, bare, "branch", "local-only", "refs/remotes/origin/"+f.defaultBranch)

			for _, name := range []string{"tag-only", "local-only", "missing"} {
				if _, err := f.pinned(t.TempDir(), name); err == nil {
					t.Errorf("pinned checkout of %q, which is not an origin branch, succeeded", name)
				}
			}
		})
	}
}

func TestRepoIdentity(t *testing.T) {
	same := []string{
		"https://github.com/Cheese-Work/Multica.git",
		"https://github.com/cheese-work/multica",
		"git@github.com:cheese-work/multica.git",
		"ssh://git@github.com/cheese-work/multica.git",
	}
	want := RepoIdentity(same[0])
	if want != "github.com/cheese-work/multica" {
		t.Fatalf("identity = %q", want)
	}
	for _, u := range same {
		if got := RepoIdentity(u); got != want {
			t.Errorf("RepoIdentity(%q) = %q, want %q", u, got, want)
		}
	}
	for _, u := range []string{"https://github.com/cheese-work/other.git", "https://gitlab.com/cheese-work/multica.git", ""} {
		if RepoIdentity(u) == want {
			t.Errorf("RepoIdentity(%q) equals the PR repository", u)
		}
	}
}

// Two repositories that share a basename and a branch name. A workdir holding
// one repository's pinned checkout must not be reused, repointed or reported
// as the other's.
func TestPinnedCheckoutRefusesAnotherRepositoryInTheSameWorkdir(t *testing.T) {
	for _, mode := range checkoutModes {
		t.Run(mode.name, func(t *testing.T) {
			root := t.TempDir()
			repos := map[string]string{}
			for _, org := range []string{"org-a", "org-b"} {
				dir := filepath.Join(root, org, "repo")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				createTestRepoAt(t, dir)
				runGitAuthored(t, dir, "checkout", "-b", pinnedBranch)
				if err := os.WriteFile(filepath.Join(dir, "who.txt"), []byte(org+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				runGitAuthored(t, dir, "add", "who.txt")
				runGitAuthored(t, dir, "commit", "-m", org)
				runGitAuthored(t, dir, "checkout", "-")
				repos[org] = dir
			}
			cache := New(t.TempDir(), testLogger())
			if err := cache.Sync("ws-1", []RepoInfo{{URL: repos["org-a"]}, {URL: repos["org-b"]}}); err != nil {
				t.Fatal(err)
			}
			workDir := t.TempDir()
			pin := func(repo string) (*WorktreeResult, error) {
				return cache.CreateWorktree(WorktreeParams{
					WorkspaceID: "ws-1", RepoURL: repo, WorkDir: workDir, Ref: pinnedBranch, Branch: pinnedBranch,
					AgentName: "Agent", TaskID: firstTaskID, IsolatedGitMetadata: mode.isolated,
				})
			}

			b, err := pin(repos["org-b"])
			if err != nil {
				t.Fatalf("checkout of B: %v", err)
			}
			if mode.isolated {
				// B's unpushed commit must not let the branch match carry the checkout over to A.
				addEmptyCommit(t, b.Path, "B local work")
			}
			headBefore := gitHead(t, b.Path)
			originBefore, _ := runGitOutput("-C", b.Path, "config", "--get", "remote.origin.url")

			if _, err := pin(repos["org-a"]); err == nil {
				t.Fatal("pinned checkout for A reused B's checkout")
			}
			if got := gitHead(t, b.Path); got != headBefore {
				t.Fatalf("B's HEAD moved: %s -> %s", headBefore, got)
			}
			if got := readFile(t, filepath.Join(b.Path, "who.txt")); got != "org-b\n" {
				t.Fatalf("B's files changed: %q", got)
			}
			originAfter, _ := runGitOutput("-C", b.Path, "config", "--get", "remote.origin.url")
			if string(originAfter) != string(originBefore) {
				t.Fatalf("B's origin rewritten: %q -> %q", originBefore, originAfter)
			}
		})
	}
}
