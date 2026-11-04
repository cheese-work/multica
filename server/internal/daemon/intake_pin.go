package daemon

import (
	"strings"
	"unicode"

	"github.com/multica-ai/multica/server/internal/daemon/repocache"
)

// Intake-run pinning (CHE-2216 plan 3.6). A run an intake item dispatched works
// on the same-repository PR's head ref and no other. The server resolves the
// pin at claim; the daemon enforces it here, so a fork ref never reaches the
// checkout or the push broker.

// validPinnedBranch accepts only a plain branch name. Anything git itself would
// reject, or that could read as a ref, option or remote:branch pair, is refused.
func validPinnedBranch(name string) bool {
	if name == "" || strings.HasPrefix(name, "-") || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") ||
		strings.HasPrefix(name, "refs/") || strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, ".") ||
		strings.Contains(name, "..") || strings.Contains(name, "//") || strings.Contains(name, "@{") {
		return false
	}
	return !strings.ContainsFunc(name, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || strings.ContainsRune(":?*[\\~^", r)
	})
}

// pinAllowsRef reports whether ref is exactly the pinned head ref. The empty or
// malformed pin allows nothing; the push broker (A3a) calls this for every
// fetch and push.
func pinAllowsRef(pinnedBranch, ref string) bool {
	return validPinnedBranch(pinnedBranch) && ref == "refs/heads/"+pinnedBranch
}

// pinAllowsRepo reports whether repoURL is the pinned PR repository. The empty
// pin allows nothing.
func pinAllowsRepo(pinnedRepo, repoURL string) bool {
	return pinnedRepo != "" && repocache.RepoIdentity(repoURL) == pinnedRepo
}
