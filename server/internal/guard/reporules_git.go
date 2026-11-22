package guard

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// CHE-7066: the merge queue reads the rules file through Git, not REST. A queue
// build made two REST calls per run, and 29 of 171 runs on 17 Nov 2026 failed
// on the installation rate limit, ejecting a PR each time. The Git read is
// the same read: the file at the live tip of the protected base branch.
const (
	rulesViaGitEnv = "GUARD_RULES_VIA_GIT" // set to 1 by pr-guard-rules.yml for a merge_group run
	gitReadTimeout = 30 * time.Second
	// A credential helper that reads GH_TOKEN from the environment at run
	// time, so the token is never on a command line.
	gitCredHelper = `!f() { echo username=x-access-token; echo "password=$GH_TOKEN"; }; f`
)

// queueRefRe matches the merge-group head ref: gh-readonly-queue/<base>/pr-<n>-<sha>.
var queueRefRe = regexp.MustCompile(`^(?:refs/heads/)?gh-readonly-queue/(.+)/pr-[0-9]+-[0-9a-f]{7,40}$`)

func (e *Engine) getenv(k string) string {
	if e.Env == nil {
		return ""
	}
	return e.Env(k)
}

// queueGitContext is true only in a merge_group run, for the PR whose base is
// the branch the queue ref names. GitHub runs a merge group only for a branch
// with a merge queue, so that branch is protected without a REST check.
func (e *Engine) queueGitContext(base string) bool {
	if e.getenv(rulesViaGitEnv) != "1" || e.getenv("GITHUB_EVENT_NAME") != "merge_group" {
		return false
	}
	m := queueRefRe.FindStringSubmatch(e.getenv("MERGE_GROUP_HEAD_REF"))
	return len(m) == 2 && m[1] == base
}

// fetchRulesViaGit reads RepoRulesFile at the live tip of base in repo with a
// shallow, blob-less fetch into a temporary repository. handled is false when
// Git could not answer (the caller falls back to REST). An error is a bad
// file only.
func (e *Engine) fetchRulesViaGit(ctx context.Context, repo, base string) (rules *Rules, revs []string, handled bool, err error) {
	var lastErr string // stderr of the last failed git call, for the fallback line
	fallback := func(why string) (*Rules, []string, bool, error) {
		if lastErr != "" {
			why += ": " + lastErr
		}
		fmt.Fprintf(e.Err, "guard: reading %s through git failed (%s); using the REST API\n", RepoRulesFile, why)
		return nil, nil, false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, gitReadTimeout)
	defer cancel()
	dir, derr := os.MkdirTemp("", "guard-rules-")
	if derr != nil {
		return fallback("temp dir: " + derr.Error())
	}
	defer os.RemoveAll(dir)

	server := strings.TrimRight(e.getenv("GITHUB_SERVER_URL"), "/")
	if server == "" {
		server = "https://github.com"
	}
	run := func(args ...string) (string, bool) {
		res, rerr := e.Runner.Run(ctx, "git", args...)
		ok := rerr == nil && res.Exit == 0
		if !ok {
			lastErr = firstLine(res.Stderr, 160)
			if rerr != nil {
				lastErr = firstLine(rerr.Error(), 160)
			}
		}
		return strings.TrimSpace(res.Stdout), ok
	}
	g := func(args ...string) (string, bool) { return run(append([]string{"-C", dir}, args...)...) }
	// Every call that can reach the network carries the helper: ls-tree and
	// cat-file lazily fetch missing objects from the blob-less remote.
	auth := []string{"-c", "credential.helper="}
	if e.getenv("GH_TOKEN") != "" {
		auth = append(auth, "-c", "credential.helper="+gitCredHelper)
	}
	net := func(args ...string) (string, bool) { return g(append(append([]string(nil), auth...), args...)...) }

	if _, ok := run("init", "-q", dir); !ok {
		return fallback("git init")
	}
	if _, ok := g("remote", "add", "origin", server+"/"+repo+".git"); !ok {
		return fallback("git remote add")
	}
	if _, ok := net("fetch", "--no-tags", "--depth=1", "--filter=blob:none", "origin", "refs/heads/"+base); !ok {
		return fallback("git fetch")
	}
	tip, ok := g("rev-parse", "FETCH_HEAD")
	if !ok || !shaRe.MatchString(tip) {
		return fallback("git rev-parse")
	}
	entry, ok := net("ls-tree", tip, "--", RepoRulesFile)
	if !ok {
		return fallback("git ls-tree")
	}
	if entry == "" {
		e.rulesNote = fmt.Sprintf("repo rules: no %s at base tip %s of %s (read through git)", RepoRulesFile, tip, repo)
		return nil, nil, true, nil
	}
	if !strings.HasPrefix(entry, "100") || !strings.Contains(entry, " blob ") { // a symlink or a directory is not the file
		return fallback("not a regular file")
	}
	raw, ok := net("cat-file", "-p", tip+":"+RepoRulesFile)
	if !ok {
		return fallback("git cat-file")
	}
	rules, revs, perr := ParseRepoRules([]byte(raw))
	if perr != nil {
		return nil, nil, true, &rulesError{bad: true, msg: fmt.Sprintf("%s at base tip %s of %s is malformed: %s", RepoRulesFile, tip[:12], repo, perr)}
	}
	e.rulesNote = fmt.Sprintf("repo rules: %s at base tip %s of %s (read through git)", RepoRulesFile, tip, repo)
	return rules, revs, true, nil
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
