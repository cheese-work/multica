// Package configsync is layer 1 of "merge is apply" (CHE-3281 design v4): it
// decides what a merged config-repo PR would do to each mapped target and
// writes nothing. Authorization comes only from GitHub API reads through the
// Reader; no webhook payload field is an input here.
package configsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Settings are the owner-only enrollment values (design step 1).
type Settings struct {
	Repo          string   `json:"repo"` // owner/name
	DefaultBranch string   `json:"default_branch"`
	ManifestPath  string   `json:"manifest_path"`
	OwnerLogins   []string `json:"owner_logins"`
}

// PullRequest holds only the API fields authorization may use.
type PullRequest struct {
	Merged         bool
	MergedByLogin  string
	MergedByType   string // GitHub account type, "Bot" for apps
	BaseRef        string
	MergeCommitSHA string
}

type Commit struct {
	Parents []string
	Message string
}

// Reader is the GitHub App read surface, bound to one repo and installation.
type Reader interface {
	PullRequest(ctx context.Context, number int) (PullRequest, error)
	TimelineEvents(ctx context.Context, number int) ([]string, error)
	// PRCommitMessages lists the PR's own commits, oldest first.
	PRCommitMessages(ctx context.Context, number int) ([]string, error)
	Commit(ctx context.Context, sha string) (Commit, error)
	// File returns found=false when the path does not exist at ref.
	File(ctx context.Context, ref, path string) (data []byte, found bool, err error)
}

// Targets reads the live governed values of the enrolling workspace.
type Targets interface {
	// LiveDigest returns found=false when the target is not in the workspace.
	LiveDigest(ctx context.Context, target string) (digest string, found bool, err error)
}

type TargetResult struct {
	Target, File, Result string
	FileDigest           string
}

type Receipt struct {
	PR          int
	MergeSHA    string
	PreMergeSHA string
	Skipped     string // whole-PR result; empty when targets were evaluated
	Targets     []TargetResult
}

const (
	TargetWorkspaceContext = "workspace.context"
	agentPrefix            = "agent:"
	// Character caps from scripts/instruction-manifest/check_sizes.py (CHE-2254).
	agentCap     = 4000
	workspaceCap = 10000
	// A rebase merge is recognised by walking back this many PR commits at most.
	maxRebaseWalk = 250
)

// Digest is SHA-256 hex of the raw bytes, with no normalisation: the same
// input and algorithm as the owner CAS statement (ownerCASWrite).
func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type manifestEntry struct {
	Target string `json:"target"`
	File   string `json:"file"`
}

// parseManifest reads {"entries":[{"target","file"}]}. Any malformed or
// duplicate entry rejects the whole manifest.
func parseManifest(b []byte) ([]manifestEntry, error) {
	var m struct {
		Entries []manifestEntry `json:"entries"`
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, e := range m.Entries {
		if e.Target == "" || e.File == "" || seen[e.Target] ||
			path.IsAbs(e.File) || path.Clean(e.File) != e.File || strings.HasPrefix(e.File, "../") || e.File == ".." {
			return nil, fmt.Errorf("bad entry %q", e.Target)
		}
		seen[e.Target] = true
	}
	return m.Entries, nil
}

// targetCap returns the character cap, or 0 for a target type outside slice 1.
func targetCap(target string) int {
	if target == TargetWorkspaceContext {
		return workspaceCap
	}
	if id, ok := strings.CutPrefix(target, agentPrefix); ok {
		if _, err := uuid.Parse(id); err == nil {
			return agentCap
		}
	}
	return 0
}

func isBot(pr PullRequest) bool {
	return strings.EqualFold(pr.MergedByType, "Bot") || strings.HasSuffix(strings.ToLower(pr.MergedByLogin), "[bot]")
}

func isOwner(s Settings, login string) bool {
	for _, o := range s.OwnerLogins {
		if login != "" && strings.EqualFold(o, login) {
			return true
		}
	}
	return false
}

// Evaluate re-reads the PR through gh and returns what each mapped target
// would get. It never writes.
func Evaluate(ctx context.Context, gh Reader, tg Targets, s Settings, number int) Receipt {
	r := Receipt{PR: number}
	skip := func(why string) Receipt { r.Skipped = why; return r }
	pr, err := gh.PullRequest(ctx, number)
	if err != nil {
		return skip("failed: fetch")
	}
	r.MergeSHA = pr.MergeCommitSHA
	switch {
	case !pr.Merged || pr.MergeCommitSHA == "":
		return skip("skipped: not merged")
	case pr.BaseRef != s.DefaultBranch:
		return skip("skipped: wrong base branch")
	case !isOwner(s, pr.MergedByLogin):
		return skip("skipped: not owner merge")
	case isBot(pr):
		return skip("skipped: not direct merge")
	}
	events, err := gh.TimelineEvents(ctx, number)
	if err != nil {
		return skip("failed: fetch")
	}
	for _, e := range events {
		if e == "auto_merge_enabled" || e == "added_to_merge_queue" {
			return skip("skipped: not direct merge")
		}
	}
	if r.PreMergeSHA, err = preMergeCommit(ctx, gh, number, pr.MergeCommitSHA); err != nil {
		return skip("failed: fetch")
	}
	pre, preFound, err := gh.File(ctx, r.PreMergeSHA, s.ManifestPath)
	if err != nil {
		return skip("failed: fetch")
	}
	merged, mergedFound, err := gh.File(ctx, pr.MergeCommitSHA, s.ManifestPath)
	if err != nil {
		return skip("failed: fetch")
	}
	var preEntries, mergedEntries []manifestEntry
	if preFound {
		if preEntries, err = parseManifest(pre); err != nil {
			return skip("skipped: invalid manifest")
		}
	}
	if mergedFound {
		if mergedEntries, err = parseManifest(merged); err != nil {
			return skip("skipped: invalid manifest")
		}
	}
	// Write targets come only from the pre-merge manifest, so a PR cannot
	// widen its own targets. Targets it newly maps are onboarding only.
	inPre := map[string]bool{}
	for _, e := range preEntries {
		inPre[e.Target] = true
		if res, ok := evaluateTarget(ctx, gh, tg, r, e); ok {
			r.Targets = append(r.Targets, res)
		}
	}
	for _, e := range mergedEntries {
		if !inPre[e.Target] {
			r.Targets = append(r.Targets, TargetResult{Target: e.Target, File: e.File, Result: "onboarding (no write in L1)"})
		}
	}
	return r
}

// evaluateTarget returns ok=false when the PR did not change the mapped file.
func evaluateTarget(ctx context.Context, gh Reader, tg Targets, r Receipt, e manifestEntry) (TargetResult, bool) {
	res := TargetResult{Target: e.Target, File: e.File}
	before, beforeFound, err1 := gh.File(ctx, r.PreMergeSHA, e.File)
	after, afterFound, err2 := gh.File(ctx, r.MergeSHA, e.File)
	if err1 != nil || err2 != nil {
		res.Result = "failed: fetch"
		return res, true
	}
	if beforeFound == afterFound && string(before) == string(after) {
		return res, false
	}
	capChars := targetCap(e.Target)
	switch {
	case capChars == 0:
		res.Result = "skipped: target type"
		return res, true
	case !afterFound:
		res.Result = "skipped: deleted"
		return res, true
	case !utf8.Valid(after) || strings.ContainsRune(string(after), 0):
		res.Result = "skipped: invalid encoding"
		return res, true
	case utf8.RuneCount(after) > capChars:
		res.Result = "skipped: over cap"
		return res, true
	}
	res.FileDigest = Digest(after)
	live, found, err := tg.LiveDigest(ctx, e.Target)
	switch {
	case err != nil:
		res.Result = "failed: fetch"
	case !found:
		res.Result = "skipped: target not in workspace"
	case live == res.FileDigest:
		res.Result = "in sync"
	case beforeFound && live == Digest(before):
		res.Result = "would apply"
	default:
		res.Result = "drift"
	}
	return res, true
}

// preMergeCommit is the default-branch commit just before the merge: the first
// parent for merge commits and squash merges; for a rebase merge, the parent of
// the first rebased commit. A rebase is recognised when the PR has several
// commits and the commits ending at the merge commit carry the same messages.
// Unreadable or unprovable history is an error, never a guess: inside a rebased
// PR the merge commit's parent is a PR commit, so guessing would trust the PR's
// own mappings as pre-merge.
func preMergeCommit(ctx context.Context, gh Reader, number int, mergeSHA string) (string, error) {
	c, err := gh.Commit(ctx, mergeSHA)
	if err != nil {
		return "", err
	}
	if len(c.Parents) == 0 {
		return "", fmt.Errorf("merge commit has no parent")
	}
	if len(c.Parents) > 1 {
		return c.Parents[0], nil
	}
	msgs, err := gh.PRCommitMessages(ctx, number)
	if err != nil {
		return "", err
	}
	if len(msgs) == 0 || len(msgs) >= maxRebaseWalk {
		return "", fmt.Errorf("cannot prove the merge method: PR has %d commits", len(msgs))
	}
	if len(msgs) == 1 {
		return c.Parents[0], nil
	}
	cur, walked := c, []string{c.Message}
	for len(walked) < len(msgs) {
		if len(cur.Parents) != 1 {
			return c.Parents[0], nil // a merge or root commit inside the walk: not a rebase
		}
		if cur, err = gh.Commit(ctx, cur.Parents[0]); err != nil {
			return "", err
		}
		walked = append(walked, cur.Message)
	}
	for i, m := range msgs { // walked is newest first, msgs oldest first
		if walked[len(msgs)-1-i] != m {
			return c.Parents[0], nil // a rebase keeps every message: this is a squash
		}
	}
	if len(cur.Parents) != 1 {
		return "", fmt.Errorf("first rebased commit has %d parents", len(cur.Parents))
	}
	return cur.Parents[0], nil
}

// RenderComment is the dry-run PR comment: one line per target. The marker
// names the merge commit so a later layer can find its own receipts.
func RenderComment(r Receipt) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<!-- config-sync dry-run %s -->\n**Config sync, dry run.** Nothing was written.\n\nMerge commit: `%s`", r.MergeSHA, r.MergeSHA)
	if r.PreMergeSHA != "" {
		fmt.Fprintf(&b, ", pre-merge commit: `%s`", r.PreMergeSHA)
	}
	b.WriteString("\n\n")
	if r.Skipped != "" {
		fmt.Fprintf(&b, "Result: %s\n", r.Skipped)
		return b.String()
	}
	if len(r.Targets) == 0 {
		b.WriteString("Result: no mapped file changed.\n")
		return b.String()
	}
	b.WriteString("| Target | File | Result | File digest |\n| --- | --- | --- | --- |\n")
	for _, t := range r.Targets {
		fmt.Fprintf(&b, "| `%s` | `%s` | %s | `%s` |\n", t.Target, t.File, t.Result, t.FileDigest)
	}
	return b.String()
}

var (
	repoPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)
	loginPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)
)

const maxOwnerLogins = 10

// ParseSettings is the one validator for the owner-written settings value:
// strict JSON, a bounded owner list, and a manifest path that stays inside the
// repo. Enrollment is the trust root, so nothing malformed is stored.
func ParseSettings(raw string) (Settings, error) {
	var s Settings
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Settings{}, fmt.Errorf("settings must be a JSON object with repo, default_branch, manifest_path, owner_logins: %w", err)
	}
	if dec.More() {
		return Settings{}, errors.New("settings must be a single JSON object")
	}
	switch {
	case !repoPattern.MatchString(s.Repo) || strings.Contains(s.Repo, ".."):
		return Settings{}, errors.New("repo must be owner/name")
	case strings.TrimSpace(s.DefaultBranch) == "" || strings.ContainsAny(s.DefaultBranch, " \t\r\n"):
		return Settings{}, errors.New("default_branch must be a branch name")
	case s.ManifestPath == "" || path.IsAbs(s.ManifestPath) || path.Clean(s.ManifestPath) != s.ManifestPath || strings.HasPrefix(s.ManifestPath, ".."):
		return Settings{}, errors.New("manifest_path must be a clean relative path inside the repo")
	case len(s.OwnerLogins) == 0 || len(s.OwnerLogins) > maxOwnerLogins:
		return Settings{}, fmt.Errorf("owner_logins must hold 1 to %d logins", maxOwnerLogins)
	}
	seen := map[string]bool{}
	for _, l := range s.OwnerLogins {
		k := strings.ToLower(l)
		if !loginPattern.MatchString(l) || seen[k] {
			return Settings{}, errors.New("owner_logins must be unique GitHub logins")
		}
		seen[k] = true
	}
	return s, nil
}
