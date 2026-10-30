package repocache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A pinned checkout (WorktreeParams.Branch) is the exact branch named, based on
// that branch's own remote head. It never gets a renamed branch, a tag or local
// ref as its base, or a checkout that loses local work.

// RepoIdentity is the host/owner/name form of a repository URL, lower-cased and
// without a ".git" suffix, so the ssh and https URLs of one repository compare
// equal.
func RepoIdentity(rawURL string) string {
	host, path := splitHostAndPath(strings.TrimSpace(rawURL))
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	return strings.ToLower(host + "/" + path)
}

// resolvePinnedBaseRefContext resolves only refs/remotes/origin/<branch>.
func resolvePinnedBaseRefContext(ctx context.Context, barePath, branch string) (string, error) {
	ref := "refs/remotes/origin/" + branch
	if !gitRefExistsContext(ctx, barePath, ref+"^{commit}") {
		return "", fmt.Errorf("pinned branch %q is not on origin in repo cache at %s", branch, barePath)
	}
	return ref, nil
}

// reuseExactBranchContext reports whether refs/heads/<branch> already exists
// and is safe to check out at base. An existing branch is reused when it is
// base, fast-forwarded when base descends from it, and refused otherwise so no
// local commit is lost.
func reuseExactBranchContext(ctx context.Context, repoPath, branch, base string) (bool, error) {
	ref := "refs/heads/" + branch
	if !gitRefExistsContext(ctx, repoPath, ref) {
		return false, nil
	}
	local, err := resolveCommitContext(ctx, repoPath, ref)
	if err != nil {
		return false, err
	}
	target, err := resolveCommitContext(ctx, repoPath, base)
	if err != nil {
		return false, err
	}
	if local == target {
		return true, nil
	}
	if runGitContext(ctx, "-C", repoPath, "merge-base", "--is-ancestor", local, target) != nil {
		return false, fmt.Errorf("local branch %q holds commits its remote head lacks; refusing to move it", branch)
	}
	// Git refuses to move a branch another worktree has checked out.
	if out, err := runGitCombinedOutputContext(ctx, "-C", repoPath, "branch", "-f", branch, target); err != nil {
		return false, fmt.Errorf("git branch -f %s: %s: %w", branch, strings.TrimSpace(string(out)), err)
	}
	return true, nil
}

func checkoutExactBranchContext(ctx context.Context, repoPath, branch, base string) (string, error) {
	exists, err := reuseExactBranchContext(ctx, repoPath, branch, base)
	if err != nil {
		return "", err
	}
	args := []string{"-C", repoPath, "switch", branch}
	if !exists {
		args = []string{"-C", repoPath, "switch", "-c", branch, base}
	}
	if out, err := runGitCombinedOutputContext(ctx, args...); err != nil {
		return "", fmt.Errorf("git switch %s: %s: %w", branch, strings.TrimSpace(string(out)), err)
	}
	return branch, nil
}

func createPinnedWorktreeContext(ctx context.Context, gitRoot, worktreePath, branch, base string) (string, error) {
	if _, err := os.Stat(worktreePath); err == nil {
		return "", fmt.Errorf("worktree path already exists and is not a valid git worktree: %s", worktreePath)
	}
	exists, err := reuseExactBranchContext(ctx, gitRoot, branch, base)
	if err != nil {
		return "", err
	}
	args := []string{"-C", gitRoot, "worktree", "add", "-b", branch, worktreePath, base}
	if exists {
		args = []string{"-C", gitRoot, "worktree", "add", worktreePath, branch}
	}
	if out, err := runGitCombinedOutputContext(ctx, args...); err != nil {
		return "", fmt.Errorf("git worktree add: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return branch, nil
}

// pinnedCheckoutState says whether an existing checkout is already on the
// pinned branch and so is left as it is. A checkout on another branch that
// holds local work is refused.
func pinnedCheckoutState(state *WorktreeResult, branch string) (reuse bool, err error) {
	if state.BranchName == branch {
		state.Kept = KeptTaskBranch
		return true, nil
	}
	if state.UncommittedFiles > 0 || state.UnpushedCommits > 0 {
		return false, fmt.Errorf("checkout at %s holds local work on %q; refusing to move it to pinned branch %q", state.Path, state.BranchName, branch)
	}
	return false, nil
}

func updatePinnedCheckoutContext(ctx context.Context, path, branch, base string) (*WorktreeResult, error) {
	state, err := inspectCheckoutContext(ctx, path)
	if err != nil {
		return nil, err
	}
	reuse, err := pinnedCheckoutState(state, branch)
	if err != nil {
		return nil, err
	}
	if reuse {
		return state, nil
	}
	if _, err := checkoutExactBranchContext(ctx, path, branch, base); err != nil {
		return nil, err
	}
	return &WorktreeResult{Path: path, BranchName: branch}, nil
}

// requirePinnedIsolatedRepoContext refuses an existing isolated checkout whose
// origin is not repoURL's repository. It runs before anything changes the
// checkout, so a workdir holding another repository's checkout of the same
// basename and branch name is never repointed or reported as the pinned one.
func requirePinnedIsolatedRepoContext(ctx context.Context, path, repoURL string) error {
	out, err := runGitOutputContext(ctx, "-C", path, "config", "--get", "remote.origin.url")
	origin := strings.TrimSpace(string(out))
	if err != nil || origin == "" || RepoIdentity(origin) != RepoIdentity(repoURL) {
		return fmt.Errorf("checkout at %s belongs to another repository than %s; refusing to reuse it", path, RepoIdentity(repoURL))
	}
	return nil
}

// requirePinnedLinkedRepoContext refuses an existing linked worktree that is
// not a worktree of this repository's cache.
func requirePinnedLinkedRepoContext(ctx context.Context, path, barePath string) error {
	out, err := runGitOutputContext(ctx, "-C", path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	common := strings.TrimSpace(string(out))
	if err != nil || common == "" || !samePath(common, barePath) {
		return fmt.Errorf("checkout at %s belongs to another repository than the cache at %s; refusing to reuse it", path, barePath)
	}
	return nil
}

func samePath(a, b string) bool {
	resolve := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		return filepath.Clean(p)
	}
	return resolve(a) == resolve(b)
}
