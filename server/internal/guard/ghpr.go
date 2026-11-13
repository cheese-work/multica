package guard

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var (
	prURLRe  = regexp.MustCompile(`https?://[^\s/]+/[^\s/]+/[^\s/]+/pull/\d+`)
	prRepoRe = regexp.MustCompile(`^https?://[^/]+/([^/]+/[^/]+)/pull/`)
	remoteRe = regexp.MustCompile(`[:/]([^/:]+)/[^/]+$`)
)

const prFields = "url,title,body,isDraft,headRefName,baseRefName,headRepositoryOwner,reviewRequests"

type prInfo struct {
	URL                 string `json:"url"`
	Title               string `json:"title"`
	Body                string `json:"body"`
	IsDraft             bool   `json:"isDraft"`
	HeadRefName         string `json:"headRefName"`
	BaseRefName         string `json:"baseRefName"`
	HeadRepositoryOwner struct {
		Login string `json:"login"`
	} `json:"headRepositoryOwner"`
	ReviewRequests []struct {
		Login string `json:"login"`
		Slug  string `json:"slug"`
		Name  string `json:"name"`
	} `json:"reviewRequests"`
}

func (p *prInfo) requested() []string {
	var out []string
	for _, r := range p.ReviewRequests {
		for _, n := range []string{r.Login, r.Slug, r.Name} {
			if n != "" {
				out = append(out, n)
			}
		}
	}
	return out
}

func hasFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

func missingFrom(want, have []string) []string {
	var out []string
	for _, w := range want {
		if !hasFold(have, w) && !hasFold(out, w) {
			out = append(out, w)
		}
	}
	return out
}

func hasTag(title string) bool {
	return strings.HasSuffix(title, " [merge]") || strings.HasSuffix(title, " [squash]")
}

// allowed reports whether the plugin may touch flag: its allow_flags, or every
// flag of the built-in when none are listed.
func allowed(pl *Plugin, flag string) bool {
	if len(pl.AllowFlags) > 0 {
		return contains(pl.AllowFlags, flag)
	}
	return contains(builtinFlags[pl.Builtin], flag)
}

// ghJSON runs gh and decodes its JSON output. A nonzero exit is an error.
func (e *Engine) ghJSON(ctx context.Context, v any, args ...string) error {
	res, err := e.Runner.Run(ctx, "gh", args...)
	if err != nil {
		return err
	}
	if res.Exit != 0 {
		return fmt.Errorf("gh %s: %s", strings.Join(leadingWords(args), " "), firstLine(res.Stderr, 120))
	}
	return json.Unmarshal([]byte(res.Stdout), v)
}

func (e *Engine) git(ctx context.Context, args ...string) (string, bool) {
	res, err := e.Runner.Run(ctx, "git", args...)
	return strings.TrimSpace(res.Stdout), err == nil && res.Exit == 0
}

func (e *Engine) view(ctx context.Context, sel ...string) (*prInfo, error) {
	args := append(append([]string{"pr", "view"}, sel...), "--json", prFields)
	var p prInfo
	if err := e.ghJSON(ctx, &p, args...); err != nil {
		return nil, err
	}
	return &p, nil
}

// tagFor picks the merge-method tag: [merge] for sync/*, merge/* and upstream/*
// branches and for a branch whose tip is a merge commit with its first parent
// already in the base; [squash] otherwise.
func (e *Engine) tagFor(ctx context.Context, head, base string) string {
	for _, p := range []string{"sync/", "merge/", "upstream/"} {
		if strings.HasPrefix(head, p) {
			return "[merge]"
		}
	}
	if out, ok := e.git(ctx, "rev-list", "--parents", "-n", "1", head); ok {
		if f := strings.Fields(out); len(f) >= 3 {
			if _, ok := e.git(ctx, "merge-base", "--is-ancestor", f[1], "origin/"+base); ok {
				return "[merge]"
			}
		}
	}
	return "[squash]"
}

func (e *Engine) ghPR(ctx context.Context, pl *Plugin, args []string) (decision, error) {
	if len(args) < 2 {
		return pass(args), nil
	}
	switch args[1] {
	case "create":
		return e.prCreate(ctx, pl, args)
	case "edit":
		return e.prEdit(ctx, pl, args)
	case "ready":
		return e.prReady(ctx, pl, args)
	}
	return pass(args), nil
}

// headOwner finds who owns the remote branch is pushed to, or "" when unknown.
func (e *Engine) headOwner(ctx context.Context, branch string) string {
	remote, ok := e.git(ctx, "config", "--get", "branch."+branch+".remote")
	if !ok || remote == "" || remote == "." {
		return ""
	}
	url, ok := e.git(ctx, "remote", "get-url", remote)
	if !ok {
		return ""
	}
	if m := remoteRe.FindStringSubmatch(strings.TrimSuffix(strings.TrimSuffix(url, "/"), ".git")); m != nil {
		return m[1]
	}
	return ""
}

// createPreflightFailed refuses a create when a check before it failed.
// Running gh pr create unchecked could open a non-draft PR; nothing exists yet,
// so a retry is safe. With verify off the call goes through unchecked.
func (e *Engine) createPreflightFailed(err error, what string) (decision, error) {
	if !e.pol().verify() {
		return decision{}, err
	}
	return refuse(2, "gh pr create stopped: could not %s (%s); nothing was created, retry the command", what, firstLine(err.Error(), 120)), nil
}

func (e *Engine) prCreate(ctx context.Context, pl *Plugin, args []string) (decision, error) {
	flags, _ := prSpec.scan(args)
	for _, n := range []string{"fill", "fill-first", "fill-verbose", "web"} {
		if isOn(flags, n) {
			return refuse(2, "gh pr create --%s is not allowed; pass --title and --body so the PR gets a tagged title", n), nil
		}
	}
	title, ok := find(flags, "title")
	if !ok || strings.TrimSpace(title.value) == "" {
		return refuse(2, "gh pr create needs a non-empty --title; the guard adds the [merge] or [squash] tag to it"), nil
	}
	if d, ok := find(flags, "draft"); ok && d.hasVal && d.value == "false" && e.pol().verify() {
		return refuse(2, "gh pr create must open a draft PR; remove --draft=false and run gh pr ready when it is done"), nil
	}

	repoView := []string{"repo", "view"}
	if r, ok := find(flags, "repo"); ok && r.hasVal {
		repoView = append(repoView, "--repo", r.value)
	}
	var rv struct {
		NameWithOwner    string `json:"nameWithOwner"`
		DefaultBranchRef struct {
			Name string `json:"name"`
		} `json:"defaultBranchRef"`
	}
	if err := e.ghJSON(ctx, &rv, append(repoView, "--json", "nameWithOwner,defaultBranchRef")...); err != nil {
		return e.createPreflightFailed(err, "read the repository")
	}
	base := rv.DefaultBranchRef.Name
	if b, ok := find(flags, "base"); ok && b.hasVal {
		base = b.value
	}

	branch, owner := "", ""
	if h, ok := find(flags, "head"); ok && h.hasVal {
		if o, b, cut := strings.Cut(h.value, ":"); cut {
			owner, branch = o, b
		} else {
			branch = h.value
		}
	}
	if branch == "" {
		b, ok := e.git(ctx, "branch", "--show-current")
		if !ok || b == "" {
			return e.createPreflightFailed(fmt.Errorf("cannot tell which branch the PR comes from"), "tell which branch the PR comes from")
		}
		branch = b
	}
	if owner == "" {
		owner = e.headOwner(ctx, branch)
	}

	// Open PRs from this branch into this base. Fork branches share names, so
	// the head owner is part of the identity when it is known.
	list := func() ([]prInfo, error) {
		var all, mine []prInfo
		err := e.ghJSON(ctx, &all, "pr", "list", "--repo", rv.NameWithOwner, "--head", branch, "--state", "open",
			"--json", "number,"+prFields, "--limit", "100")
		for _, p := range all {
			if p.BaseRefName == base && (owner == "" || strings.EqualFold(p.HeadRepositoryOwner.Login, owner)) {
				mine = append(mine, p)
			}
		}
		return mine, err
	}
	found, err := list()
	if err != nil {
		return e.createPreflightFailed(err, "list the open PRs")
	}
	if len(found) > 0 && owner == "" {
		return refuse(2, "an open PR from a branch named %s exists and the guard cannot tell whose it is; pass --head owner:%s", branch, branch), nil
	}
	if len(found) > 0 {
		return e.finishCreate(ctx, pl, found[0].URL, false, []string{"reused the PR that already exists"}), nil
	}

	pol, rerr := e.effective(ctx, rv.NameWithOwner, base)
	if rerr != nil {
		if d, ok := rulesRefusal(rerr, "gh pr create"); ok {
			return d, nil
		}
		return e.createPreflightFailed(rerr, "read the repository rules")
	}

	tag := e.tagFor(ctx, branch, base)
	newArgs := append([]string(nil), args...)
	var fixes []string
	if !hasTag(title.value) && allowed(pl, "--title") {
		newArgs = withValue(newArgs, title, title.value+" "+tag)
		fixes = append(fixes, "added "+tag+" to the title")
	}
	// The body is checked once the PR exists, in ensure.
	if bad := pol.Rules.titleProblems(title.value); len(bad) > 0 {
		return refuse(2, "gh pr create stopped: %s; nothing was created, fix --title", strings.Join(bad, "; ")), nil
	}
	if missing := missingFrom(pol.githubReviewers(rv.NameWithOwner), values(flags, "reviewer")); len(missing) > 0 && allowed(pl, "--reviewer") {
		if rf, ok := find(flags, "reviewer"); ok && rf.hasVal {
			newArgs = withValue(newArgs, rf, rf.value+","+strings.Join(missing, ","))
		} else {
			newArgs = append(newArgs, "--reviewer", strings.Join(missing, ","))
		}
		fixes = append(fixes, "requested reviewers "+strings.Join(missing, ", "))
	}
	if e.pol().verify() && allowed(pl, "--draft") && !isOn(flags, "draft") {
		newArgs = append(newArgs, "--draft")
		fixes = append([]string{"made it a draft"}, fixes...)
	}

	res, err := e.Runner.Run(ctx, "gh", newArgs...)
	if err != nil {
		return decision{}, err
	}
	url := prURL(res.Stdout)
	if res.Exit != 0 || url == "" {
		// The PR may exist although gh failed: a retry, a concurrent caller, or a
		// reviewer request that failed after the PR was made. Recover it.
		again, lerr := list()
		if lerr != nil || len(again) == 0 || (owner == "" && len(again) > 1) {
			fmt.Fprint(e.Out, res.Stdout)
			fmt.Fprint(e.Err, res.Stderr)
			if res.Exit == 0 {
				return refuse(1, "gh pr create ended without a PR url; check with gh pr list --head %s", branch), nil
			}
			return decision{kind: KindPass, code: res.Exit, done: true}, nil
		}
		return e.finishCreate(ctx, pl, again[0].URL, true, fixes), nil
	}
	fmt.Fprint(e.Err, res.Stderr)
	return e.finishCreate(ctx, pl, url, true, fixes), nil
}

func prURL(out string) string { return prURLRe.FindString(out) }

// finishCreate reads the PR back, repairs a missing tag or reviewer, and
// prints the url only when the PR passes.
func (e *Engine) finishCreate(ctx context.Context, pl *Plugin, url string, fresh bool, fixes []string) decision {
	d := decision{kind: KindFixed, done: true, line: strings.Join(fixes, "; ")}
	if e.pol().verify() {
		if d = e.ensure(ctx, pl, url, fresh, 1, fixes); d.kind == KindRefused {
			return d
		}
	}
	fmt.Fprintln(e.Out, url)
	return d
}

// ensure reads the PR back and repairs what the rules need: the title tag and
// the policy reviewers. With wantDraft it also requires a draft. A PR that
// still fails is a refusal with failCode and a line naming the PR.
func (e *Engine) ensure(ctx context.Context, pl *Plugin, url string, wantDraft bool, failCode int, fixes []string) decision {
	fail := func(format string, a ...any) decision {
		return refuse(failCode, "PR %s %s", url, fmt.Sprintf(format, a...))
	}
	info, err := e.view(ctx, url)
	if err != nil {
		return fail("exists, but the guard could not read it back (%s); check it with gh pr view", firstLine(err.Error(), 120))
	}
	repo := ""
	if m := prRepoRe.FindStringSubmatch(info.URL); m != nil {
		repo = m[1]
	}
	pol, rerr := e.effective(ctx, repo, info.BaseRefName)
	if rerr != nil {
		// The rules could not be read, so the PR was not judged: check-pr refuses (2), never reports a broken rule (1).
		if e.noRepair {
			failCode = 2
		}
		if isBadRules(rerr) {
			return fail("cannot be checked: %s; fix the file on the base branch", rerr)
		}
		return fail("cannot be checked: %s; it stays as it is, retry once the rules can be read", rerr)
	}
	wantRev := pol.githubReviewers(repo)
	needTag := func(p *prInfo) bool { return !hasTag(p.Title) && allowed(pl, "--title") }
	needRev := func(p *prInfo) []string {
		if !allowed(pl, "--add-reviewer") {
			return nil
		}
		return missingFrom(wantRev, p.requested())
	}

	var edit, repaired []string
	if needTag(info) {
		tag := e.tagFor(ctx, info.HeadRefName, info.BaseRefName)
		edit = append(edit, "--title", info.Title+" "+tag)
		repaired = append(repaired, "added "+tag+" to the title")
	}
	if m := needRev(info); len(m) > 0 {
		edit = append(edit, "--add-reviewer", strings.Join(m, ","))
		repaired = append(repaired, "requested reviewers "+strings.Join(m, ", "))
	}
	if len(edit) > 0 && !e.noRepair {
		res, rerr := e.Runner.Run(ctx, "gh", append([]string{"pr", "edit", url}, edit...)...)
		if rerr == nil && res.Exit == 0 {
			fixes = append(fixes, repaired...)
		}
		if info, err = e.view(ctx, url); err != nil {
			return fail("was edited, but the guard could not read it back (%s); check it with gh pr view", firstLine(err.Error(), 120))
		}
	}

	var problems []string
	if wantDraft && !info.IsDraft {
		problems = append(problems, "it is not a draft")
	}
	if m := needRev(info); len(m) > 0 {
		if e.noRepair {
			problems = append(problems, "reviewers missing ("+strings.Join(m, ", ")+")")
		} else {
			problems = append(problems, "reviewers missing ("+strings.Join(m, ", ")+") and the request failed")
		}
	}
	if needTag(info) {
		problems = append(problems, "the title has no [merge] or [squash] tag")
	}
	problems = append(problems, pol.Rules.problems(info.Title, info.Body)...)
	if len(problems) > 0 {
		return fail("fails the PR rules: %s. Fix it with gh pr edit %s", strings.Join(problems, "; "), url)
	}
	kind := KindPass
	if len(fixes) > 0 {
		kind = KindFixed
	}
	return decision{kind: kind, done: true, line: strings.Join(fixes, "; ")}
}

// selector turns the positional words after "pr <verb>" and a --repo flag into
// the arguments `gh pr view` takes to find the same PR.
func selector(flags []flagTok, pos []string) []string {
	sel := append([]string(nil), pos...)
	if r, ok := find(flags, "repo"); ok && r.hasVal {
		sel = append(sel, "--repo", r.value)
	}
	return sel
}

func (e *Engine) prEdit(ctx context.Context, pl *Plugin, args []string) (decision, error) {
	flags, pos := prSpec.scan(args)
	title, ok := find(flags, "title")
	if !ok || !title.hasVal || strings.TrimSpace(title.value) == "" || hasTag(title.value) || !allowed(pl, "--title") {
		return pass(args), nil
	}
	info, err := e.view(ctx, selector(flags, pos[2:])...)
	if err != nil {
		return decision{}, err
	}
	tag := e.tagFor(ctx, info.HeadRefName, info.BaseRefName)
	return decision{kind: KindFixed, line: "added " + tag + " to the title",
		args: withValue(args, title, title.value+" "+tag)}, nil
}

func (e *Engine) prReady(ctx context.Context, pl *Plugin, args []string) (decision, error) {
	flags, pos := prSpec.scan(args)
	if isOn(flags, "undo") {
		return pass(args), nil
	}
	info, err := e.view(ctx, selector(flags, pos[2:])...)
	if err != nil {
		if !e.pol().verify() {
			return decision{}, err
		}
		return refuse(2, "gh pr ready stopped: could not read the PR (%s); it stays draft, retry the command", firstLine(err.Error(), 120)), nil
	}
	d := e.ensure(ctx, pl, info.URL, false, 2, nil)
	if d.kind == KindRefused {
		return d, nil
	}
	return decision{kind: d.kind, line: d.line, args: args}, nil
}

// ghAPI refuses writes to pull requests, issues and git data through `gh api`,
// because they skip the checks on gh pr and gh issue.
func ghAPI(args []string) decision {
	flags, pos := apiSpec.scan(args)
	method := "GET"
	if _, ok := find(flags, "field"); ok {
		method = "POST"
	}
	for _, n := range []string{"raw-field", "input"} {
		if _, ok := find(flags, n); ok {
			method = "POST"
		}
	}
	for _, f := range flags {
		if f.name == "method" && f.hasVal {
			method = strings.ToUpper(f.value)
		}
	}
	if method == "GET" || method == "HEAD" || len(pos) < 2 {
		return pass(args)
	}
	parts := strings.Split(strings.SplitN(strings.TrimPrefix(pos[1], "/"), "?", 2)[0], "/")
	if len(parts) >= 4 && parts[0] == "repos" && (parts[3] == "pulls" || parts[3] == "issues" || parts[3] == "git") {
		return refuse(2, "gh api %s on repos/*/%s is not allowed; use gh pr create, edit or ready, or gh issue", method, parts[3])
	}
	return pass(args)
}
