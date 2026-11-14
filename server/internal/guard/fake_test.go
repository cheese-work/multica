package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakePR is one pull request in the fake GitHub.
type fakePR struct {
	Number    int
	Repo      string // base repository, owner/name
	HeadOwner string
	HeadRef   string
	Base      string
	Title     string
	Draft     bool
	Reviewers []string
	Body      string
}

func (p *fakePR) url() string { return fmt.Sprintf("https://github.com/%s/pull/%d", p.Repo, p.Number) }

// fakeEnv is a fake gh and git. Calls are recorded as "tool arg arg".
type fakeEnv struct {
	mu sync.Mutex

	prs      []*fakePR
	calls    []string
	streamed []string

	// repository state
	defaultRepo   string
	defaultBranch string
	branch        string // checked-out branch
	remoteOwner   string // owner of the branch's push remote ("" = unknown)
	prefix        string // git rev-parse --show-prefix: cwd inside the repository
	prefixFails   bool   // git rev-parse --show-prefix fails
	merges        map[string]bool
	aliases       map[string]string

	// failure injection
	createReviewerFails bool // create makes the PR, then exits 1 for the reviewer request
	editReviewerFails   bool
	viewFails           bool
	repoViewFails       bool
	listFails           bool
	streamExit          int

	// repository rules (.github/agent-rules.yml): base tips, protection, file contents
	tips        map[string]string // "owner/name/branch" -> tip SHA
	unprotected map[string]bool   // "owner/name/branch" -> branch is not protected
	files       map[string]string // "owner/name@ref" -> file contents
	apiFails    bool              // every gh api call exits 1 with HTTP 502
}

func newFake() *fakeEnv {
	return &fakeEnv{
		defaultRepo: "cheese-work/multica", defaultBranch: "main",
		branch: "feature/x", remoteOwner: "cheese-work",
		merges: map[string]bool{}, aliases: map[string]string{},
		tips: map[string]string{}, unprotected: map[string]bool{}, files: map[string]string{},
	}
}

func (f *fakeEnv) record(tool string, args []string) {
	f.calls = append(f.calls, tool+" "+strings.Join(args, " "))
}

// callsWith counts recorded calls that contain every fragment.
func (f *fakeEnv) callsWith(frags ...string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
outer:
	for _, c := range f.calls {
		for _, fr := range frags {
			if !strings.Contains(c, fr) {
				continue outer
			}
		}
		n++
	}
	return n
}

func (f *fakeEnv) Stream(_ context.Context, tool string, args ...string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.streamed = append(f.streamed, tool+" "+strings.Join(args, " "))
	if tool == "gh" && len(args) >= 2 && args[0] == "pr" && args[1] == "ready" && f.streamExit == 0 {
		if p := f.findByArg(args[2:]); p != nil {
			p.Draft = false
		}
	}
	if tool == "gh" && len(args) >= 3 && args[0] == "pr" && args[1] == "edit" {
		f.applyEdit(f.findByArg(args[2:]), args[2:])
	}
	return f.streamExit, nil
}

func (f *fakeEnv) Run(_ context.Context, tool string, args ...string) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(tool, args)
	if tool == "git" {
		return f.git(args), nil
	}
	return f.gh(args), nil
}

func val(args []string, long, short string) string {
	for i, a := range args {
		switch {
		case a == long || (short != "" && a == short):
			if i+1 < len(args) {
				return args[i+1]
			}
		case strings.HasPrefix(a, long+"="):
			return strings.TrimPrefix(a, long+"=")
		}
	}
	return ""
}

func has(args []string, names ...string) bool {
	for _, a := range args {
		for _, n := range names {
			if a == n || strings.HasPrefix(a, n+"=") {
				return true
			}
		}
	}
	return false
}

func (f *fakeEnv) git(args []string) Result {
	switch {
	case len(args) >= 2 && args[0] == "branch" && args[1] == "--show-current":
		return Result{Stdout: f.branch + "\n"}
	case len(args) >= 3 && args[0] == "config" && strings.HasSuffix(args[len(args)-1], ".remote"):
		if f.remoteOwner == "" {
			return Result{Exit: 1}
		}
		return Result{Stdout: "origin\n"}
	case len(args) >= 2 && args[0] == "remote" && args[1] == "get-url":
		return Result{Stdout: "https://github.com/" + f.remoteOwner + "/multica.git\n"}
	case len(args) >= 1 && args[0] == "rev-list": // ancestry: "<head> is a merge into base"
		head := args[len(args)-1]
		if f.merges[head] {
			return Result{Stdout: "tip p1 p2\n"}
		}
		return Result{Stdout: "tip p1\n"}
	case len(args) >= 2 && args[0] == "rev-parse" && args[1] == "--show-prefix":
		if f.prefixFails {
			return Result{Exit: 128, Stderr: "fatal: not a git repository"}
		}
		return Result{Stdout: f.prefix + "\n"}
	case len(args) >= 1 && args[0] == "merge-base":
		return Result{Exit: 0}
	}
	return Result{Exit: 1, Stderr: "fake git: unhandled " + strings.Join(args, " ")}
}

func (f *fakeEnv) findByArg(rest []string) *fakePR {
	for _, a := range rest {
		if strings.HasPrefix(a, "-") {
			continue
		}
		if i := strings.LastIndex(a, "/"); i >= 0 {
			a = a[i+1:]
		}
		if n, err := strconv.Atoi(a); err == nil {
			for _, p := range f.prs {
				if p.Number == n {
					return p
				}
			}
		}
	}
	if len(f.prs) == 1 {
		return f.prs[0]
	}
	return nil
}

func (f *fakeEnv) applyEdit(p *fakePR, args []string) Result {
	if p == nil {
		return Result{Exit: 1, Stderr: "no pull request"}
	}
	if t := val(args, "--title", ""); t != "" {
		p.Title = t
	}
	if r := val(args, "--add-reviewer", ""); r != "" {
		if f.editReviewerFails {
			return Result{Exit: 1, Stderr: "could not request reviewer"}
		}
		p.Reviewers = append(p.Reviewers, strings.Split(r, ",")...)
	}
	return Result{Stdout: p.url() + "\n"}
}

type jsonPR struct {
	Number              int                 `json:"number"`
	URL                 string              `json:"url"`
	Title               string              `json:"title"`
	IsDraft             bool                `json:"isDraft"`
	Body                string              `json:"body"`
	HeadRefName         string              `json:"headRefName"`
	BaseRefName         string              `json:"baseRefName"`
	HeadRepositoryOwner map[string]string   `json:"headRepositoryOwner"`
	ReviewRequests      []map[string]string `json:"reviewRequests"`
}

func (p *fakePR) json() jsonPR {
	j := jsonPR{Number: p.Number, URL: p.url(), Title: p.Title, IsDraft: p.Draft, Body: p.Body,
		HeadRefName: p.HeadRef, BaseRefName: p.Base,
		HeadRepositoryOwner: map[string]string{"login": p.HeadOwner}}
	for _, r := range p.Reviewers {
		j.ReviewRequests = append(j.ReviewRequests, map[string]string{"__typename": "User", "login": r})
	}
	return j
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b) + "\n"
}

func (f *fakeEnv) gh(args []string) Result {
	if len(args) == 0 {
		return Result{Exit: 1}
	}
	switch {
	case args[0] == "alias" && len(args) > 1 && args[1] == "list":
		var b strings.Builder
		for k, v := range f.aliases {
			fmt.Fprintf(&b, "%s:\t%s\n", k, v)
		}
		return Result{Stdout: b.String()}
	case args[0] == "repo" && len(args) > 1 && args[1] == "view":
		if f.repoViewFails {
			return Result{Exit: 1, Stderr: "HTTP 502"}
		}
		repo := f.defaultRepo
		if r := val(args, "--repo", "-R"); r != "" {
			repo = r
		}
		return Result{Stdout: mustJSON(map[string]any{
			"nameWithOwner": repo, "defaultBranchRef": map[string]string{"name": f.defaultBranch}})}
	case args[0] == "api":
		return f.api(args[1:])
	case len(args) >= 2 && args[0] == "pr" && args[1] == "list":
		if f.listFails {
			return Result{Exit: 1, Stderr: "HTTP 502"}
		}
		repo := val(args, "--repo", "-R")
		head := val(args, "--head", "-H")
		out := []jsonPR{}
		for _, p := range f.prs {
			if p.Repo == repo && p.HeadRef == head {
				out = append(out, p.json())
			}
		}
		return Result{Stdout: mustJSON(out)}
	case len(args) >= 2 && args[0] == "pr" && args[1] == "view":
		if f.viewFails {
			return Result{Exit: 1, Stderr: "HTTP 502"}
		}
		p := f.findByArg(args[2:])
		if p == nil {
			return Result{Exit: 1, Stderr: "no pull requests found"}
		}
		return Result{Stdout: mustJSON(p.json())}
	case len(args) >= 2 && args[0] == "pr" && args[1] == "edit":
		return f.applyEdit(f.findByArg(args[2:]), args[2:])
	case len(args) >= 2 && args[0] == "pr" && args[1] == "create":
		return f.create(args[2:])
	}
	return Result{Exit: 1, Stderr: "fake gh: unhandled " + strings.Join(args, " ")}
}

func (f *fakeEnv) create(args []string) Result {
	repo := val(args, "--repo", "-R")
	if repo == "" {
		repo = f.defaultRepo
	}
	headOwner, headRef := f.remoteOwner, f.branch
	if h := val(args, "--head", "-H"); h != "" {
		if o, b, ok := strings.Cut(h, ":"); ok {
			headOwner, headRef = o, b
		} else {
			headRef = h
		}
	}
	for _, p := range f.prs {
		if p.Repo == repo && p.HeadOwner == headOwner && p.HeadRef == headRef {
			return Result{Exit: 1, Stderr: "a pull request for branch \"" + headRef + "\" already exists:\n" + p.url()}
		}
	}
	base := val(args, "--base", "-B")
	if base == "" {
		base = f.defaultBranch
	}
	p := &fakePR{Number: len(f.prs) + 1, Repo: repo, HeadOwner: headOwner, HeadRef: headRef, Base: base,
		Title: val(args, "--title", "-t"), Body: val(args, "--body", "-b"), Draft: has(args, "--draft", "-d")}
	f.prs = append(f.prs, p)
	if r := val(args, "--reviewer", "-r"); r != "" {
		if f.createReviewerFails {
			return Result{Exit: 1, Stderr: "could not request reviewer " + r + " for " + p.url()}
		}
		p.Reviewers = strings.Split(r, ",")
	}
	return Result{Stdout: p.url() + "\n", Stderr: "Creating pull request for " + headRef + "\n"}
}

// run executes one guarded call and returns the exit code and the guard's
// stdout and stderr.
func run(t *testing.T, f *fakeEnv, pol *Policy, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	e := &Engine{Policy: pol, Runner: f, Out: &out, Err: &errb}
	code, err := e.Exec(context.Background(), "gh", args)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	return code, out.String(), errb.String()
}

// policyWith is the built-in plugins plus a review rule.
func policyWith(rules ...ReviewRule) *Policy {
	p := DefaultPolicy()
	p.Review = rules
	return p
}

// pr returns PR i or stops the test: a missing PR is a failed expectation, not a panic.
func (f *fakeEnv) pr(t *testing.T, i int) *fakePR {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.prs) {
		t.Fatalf("PR %d does not exist (%d PRs)", i, len(f.prs))
	}
	return f.prs[i]
}

// api serves the two reads the rules loader makes: the base branch (tip and
// protection) and the rules file at a ref.
func (f *fakeEnv) api(args []string) Result {
	if f.apiFails {
		return Result{Exit: 1, Stderr: "gh: Bad Gateway (HTTP 502)"}
	}
	path := args[len(args)-1]
	path, query, _ := strings.Cut(path, "?")
	parts := strings.Split(strings.TrimPrefix(path, "repos/"), "/")
	if len(parts) >= 4 && parts[2] == "branches" {
		repo, branch := parts[0]+"/"+parts[1], strings.Join(parts[3:], "/")
		sha, ok := f.tips[repo+"/"+branch]
		if !ok { // every other branch has a tip and no rules file
			sha = "0000000000000000000000000000000000000000"
		}
		return Result{Stdout: mustJSON(map[string]any{"name": branch, "protected": !f.unprotected[repo+"/"+branch],
			"commit": map[string]string{"sha": sha}})}
	}
	if len(parts) >= 4 && parts[2] == "contents" {
		repo, ref := parts[0]+"/"+parts[1], strings.TrimPrefix(query, "ref=")
		body, ok := f.files[repo+"@"+ref]
		if !ok || strings.Join(parts[3:], "/") != RepoRulesFile {
			return Result{Exit: 1, Stderr: "gh: Not Found (HTTP 404)"}
		}
		return Result{Stdout: body}
	}
	return Result{Exit: 1, Stderr: "fake gh api: unhandled " + path}
}

// setRules publishes a rules file at the base tip of repo's branch.
func (f *fakeEnv) setRules(repo, branch, tip, content string) {
	f.tips[repo+"/"+branch] = tip
	f.files[repo+"@"+tip] = content
}
