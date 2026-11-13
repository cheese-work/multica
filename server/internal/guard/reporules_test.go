package guard

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

const (
	repo   = "cheese-work/multica"
	tipV1  = "1111111111111111111111111111111111111111"
	tipV2  = "2222222222222222222222222222222222222222"
	noRule = "version: 1\n"
)

const keyedRules = `version: 1
title:
  pattern: '^(feat|fix): '
  require: ["CHE-"]
trailer: "Refs:"
reviewers: [alice]
`

func TestParseRepoRules(t *testing.T) {
	r, revs, err := ParseRepoRules([]byte(keyedRules))
	if err != nil {
		t.Fatalf("valid file: %v", err)
	}
	if r == nil || r.Title.Pattern != "^(feat|fix): " || len(r.Title.Require) != 1 || r.Trailer != "Refs:" || len(revs) != 1 || revs[0] != "alice" {
		t.Fatalf("parsed %+v %v", r, revs)
	}

	bad := map[string]string{
		"empty file":             "",
		"no version":             "title:\n  require: [A-]\n",
		"future version":         "version: 2\n",
		"unknown key":            "version: 1\nscript: rm -rf /\n",
		"unknown title key":      "version: 1\ntitle:\n  run: x\n",
		"bad regexp":             "version: 1\ntitle:\n  pattern: '('\n",
		"token both ways":        "version: 1\ntitle:\n  require: [CHE-]\n  forbid: [CHE-]\n",
		"empty token":            "version: 1\ntitle:\n  forbid: ['']\n",
		"trailer without colon":  "version: 1\ntrailer: Refs\n",
		"trailer with newline":   "version: 1\ntrailer: \"Refs:\\nX:\"\n",
		"second document":        "version: 1\n---\nversion: 1\n",
		"not a mapping":          "- a\n- b\n",
		"reviewer with a space":  "version: 1\nreviewers: ['a b']\n",
		"too long":               "version: 1\n# " + strings.Repeat("x", 70<<10) + "\n",
		"alias bomb":             "version: 1\ntitle:\n  require: &a [x]\n  forbid: *a\n",
		"json null title entry":  "version: 1\ntitle: 5\n",
		"pattern too long":       "version: 1\ntitle:\n  pattern: '" + strings.Repeat("a", 600) + "'\n",
		"too many reviewers":     "version: 1\nreviewers: [" + strings.Repeat("a,", 40) + "b]\n",
		"duplicate mapping keys": "version: 1\nversion: 1\n",
	}
	for name, doc := range bad {
		if _, _, err := ParseRepoRules([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// guardRun runs one gh call with a decision log and returns exit, stdout, stderr and the log.
func guardRun(t *testing.T, f *fakeEnv, pol *Policy, args ...string) (int, string, string, string) {
	t.Helper()
	var out, errb, logb bytes.Buffer
	e := &Engine{Policy: pol, Runner: f, Out: &out, Err: &errb, Log: &logb}
	code, err := e.Exec(context.Background(), "gh", args)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	return code, out.String(), errb.String(), logb.String()
}

func TestCreateAppliesRulesFromTheBaseTip(t *testing.T) {
	f := newFake()
	f.setRules(repo, "main", tipV1, keyedRules)
	code, _, errs, logs := guardRun(t, f, DefaultPolicy(), "pr", "create", "--title", "feat: x (CHE-9)", "--body", "Refs: CHE-9")
	if code != 0 {
		t.Fatalf("exit %d %q", code, errs)
	}
	if got := strings.Join(f.pr(t, 0).Reviewers, ","); got != "alice" {
		t.Errorf("reviewers %q, want alice from the file", got)
	}
	if !strings.Contains(logs, tipV1) {
		t.Errorf("decision log lacks the base tip sha: %s", logs)
	}
}

func TestEditedFileInThePRIsNotUsed(t *testing.T) {
	f := newFake()
	f.setRules(repo, "main", tipV1, "version: 1\ntitle:\n  require: [\"CHE-\"]\n")
	// The PR branch weakens the rule; neither its branch name nor its ref may be read.
	f.files[repo+"@feature/x"] = noRule
	f.files[repo+"@"+tipV2] = noRule
	code, _, errs, _ := guardRun(t, f, DefaultPolicy(), "pr", "create", "--title", "feat: no key here")
	if code != 2 || !strings.Contains(errs, "CHE-") {
		t.Fatalf("a PR that drops the rule must still fail it: exit %d %q", code, errs)
	}
	if len(f.prs) != 0 {
		t.Errorf("a refused create left %d PRs", len(f.prs))
	}
	if n := f.callsWith("api", "ref=feature/x"); n != 0 {
		t.Errorf("read the file from the PR branch %d times", n)
	}
	if n := f.callsWith("api", "contents", "ref="+tipV1); n == 0 {
		t.Errorf("never read the file at the base tip")
	}
}

func TestStaleBranchGetsARuleAddedLater(t *testing.T) {
	f := newFake()
	f.setRules(repo, "main", tipV1, noRule)
	if code, _, errs, _ := guardRun(t, f, DefaultPolicy(), "pr", "create", "--title", "feat: x"); code != 0 {
		t.Fatalf("create under no rules: exit %d %q", code, errs)
	}
	// The base gains a rule after the branch was cut.
	f.setRules(repo, "main", tipV2, "version: 1\ntitle:\n  require: [\"CHE-\"]\n")
	code, _, errs, _ := guardRun(t, f, DefaultPolicy(), "pr", "ready", "1")
	if code != 2 || !strings.Contains(errs, "CHE-") {
		t.Fatalf("ready must apply the new rule: exit %d %q", code, errs)
	}
	if !f.pr(t, 0).Draft {
		t.Errorf("a refused ready left the PR ready")
	}
	if n := len(f.streamed); n != 0 {
		t.Errorf("gh pr ready ran: %v", f.streamed)
	}
}

func TestForkPRReadsTheTargetRepository(t *testing.T) {
	f := newFake()
	f.remoteOwner = "forker"
	f.setRules(repo, "main", tipV1, keyedRules)
	f.setRules("forker/multica", "main", tipV2, noRule) // the fork's own file must be ignored
	code, _, errs, _ := guardRun(t, f, DefaultPolicy(), "pr", "create", "--title", "feat: x")
	if code != 2 || !strings.Contains(errs, "CHE-") {
		t.Fatalf("the target's rule applies to a fork PR: exit %d %q", code, errs)
	}
	if n := f.callsWith("api", "repos/forker/"); n != 0 {
		t.Errorf("read rules from the fork %d times", n)
	}
}

func TestBaseChangeBetweenCreateAndReadyIsRechecked(t *testing.T) {
	f := newFake()
	f.setRules(repo, "main", tipV1, noRule)
	if code, _, errs, _ := guardRun(t, f, DefaultPolicy(), "pr", "create", "--title", "feat: x", "--body", "text"); code != 0 {
		t.Fatalf("create: exit %d %q", code, errs)
	}
	f.setRules(repo, "main", tipV2, "version: 1\ntrailer: \"Refs:\"\nreviewers: [bob]\n")
	code, _, errs, logs := guardRun(t, f, DefaultPolicy(), "pr", "ready", "1")
	if code != 2 || !strings.Contains(errs, "Refs:") {
		t.Fatalf("ready must refuse on the new trailer rule: exit %d %q", code, errs)
	}
	if !strings.Contains(logs, tipV2) || strings.Contains(logs, tipV1) {
		t.Errorf("ready must record the new base tip: %s", logs)
	}
	f.pr(t, 0).Body = "Refs: CHE-1"
	if code, _, errs, _ = guardRun(t, f, DefaultPolicy(), "pr", "ready", "1"); code != 0 {
		t.Fatalf("ready after the fix: exit %d %q", code, errs)
	}
	if got := strings.Join(f.pr(t, 0).Reviewers, ","); got != "bob" {
		t.Errorf("reviewers %q, want bob requested at ready", got)
	}
}

func TestReviewPolicyChangeIsRechecked(t *testing.T) {
	f := newFake()
	f.setRules(repo, "main", tipV1, noRule)
	if code, _, errs, _ := guardRun(t, f, DefaultPolicy(), "pr", "create", "--title", "feat: x"); code != 0 {
		t.Fatalf("create: exit %d %q", code, errs)
	}
	if len(f.pr(t, 0).Reviewers) != 0 {
		t.Fatalf("no reviewers expected yet")
	}
	// The review policy changed after that successful check.
	code, _, errs, _ := guardRun(t, f, policyWith(ReviewRule{Repo: repo, Reviewers: []string{"carol"}}), "pr", "ready", "1")
	if code != 0 {
		t.Fatalf("ready: exit %d %q", code, errs)
	}
	if got := strings.Join(f.pr(t, 0).Reviewers, ","); got != "carol" {
		t.Errorf("reviewers %q, want carol", got)
	}
}

func TestFileReviewersAddToPolicyAndSkipWins(t *testing.T) {
	f := newFake()
	f.setRules(repo, "main", tipV1, "version: 1\nreviewers: [alice]\n")
	pol := policyWith(ReviewRule{Repo: repo, Reviewers: []string{"carol"}})
	if code, _, errs, _ := guardRun(t, f, pol, "pr", "create", "--title", "feat: x"); code != 0 {
		t.Fatalf("exit %d %q", code, errs)
	}
	if got := strings.Join(f.pr(t, 0).Reviewers, ","); got != "carol,alice" {
		t.Errorf("reviewers %q, want carol,alice", got)
	}

	g := newFake()
	g.setRules(repo, "main", tipV1, "version: 1\nreviewers: [alice]\n")
	skip := policyWith(ReviewRule{Repo: repo, GitHub: "skip"})
	if code, _, errs, _ := guardRun(t, g, skip, "pr", "create", "--title", "feat: x"); code != 0 {
		t.Fatalf("exit %d %q", code, errs)
	}
	if got := g.pr(t, 0).Reviewers; len(got) != 0 {
		t.Errorf("skip must keep GitHub reviewers off, got %v", got)
	}
}

func TestMissingFileAppliesOnlyPolicyRules(t *testing.T) {
	f := newFake()
	f.tips[repo+"/main"] = tipV1 // no file at that tip
	pol := DefaultPolicy()
	pol.Rules = &Rules{Trailer: "Refs:"}
	code, _, errs, _ := guardRun(t, f, pol, "pr", "create", "--title", "feat: x", "--body", "no trailer")
	if code != 1 || !strings.Contains(errs, "Refs:") || !f.pr(t, 0).Draft {
		t.Fatalf("the workspace rule still applies: exit %d %q", code, errs)
	}
	g := newFake()
	g.tips[repo+"/main"] = tipV1
	if code, _, errs, _ := guardRun(t, g, DefaultPolicy(), "pr", "create", "--title", "feat: x"); code != 0 {
		t.Fatalf("a missing file is not an error: exit %d %q", code, errs)
	}
}

func TestMalformedFileFailsClosed(t *testing.T) {
	f := newFake()
	f.setRules(repo, "main", tipV1, "version: 1\nscript: x\n")
	code, _, errs, _ := guardRun(t, f, DefaultPolicy(), "pr", "create", "--title", "feat: x")
	if code != 2 || !strings.Contains(errs, RepoRulesFile) || !strings.Contains(errs, "script") {
		t.Fatalf("create must name the file and the schema error: exit %d %q", code, errs)
	}
	if len(f.prs) != 0 {
		t.Errorf("a refused create left a PR")
	}
	// A PR made before the file went bad is refused at ready and stays draft.
	g := newFake()
	g.setRules(repo, "main", tipV1, noRule)
	if code, _, errs, _ := guardRun(t, g, DefaultPolicy(), "pr", "create", "--title", "feat: x"); code != 0 {
		t.Fatalf("create: exit %d %q", code, errs)
	}
	g.setRules(repo, "main", tipV2, "{{{")
	code, _, errs, _ = guardRun(t, g, DefaultPolicy(), "pr", "ready", "1")
	if code != 2 || !strings.Contains(errs, RepoRulesFile) || !g.pr(t, 0).Draft {
		t.Fatalf("ready: exit %d %q draft=%v", code, errs, g.pr(t, 0).Draft)
	}
}

func TestRulesAPIErrorRefusesAndKeepsDraft(t *testing.T) {
	f := newFake()
	f.setRules(repo, "main", tipV1, noRule)
	if code, _, errs, _ := guardRun(t, f, DefaultPolicy(), "pr", "create", "--title", "feat: x"); code != 0 {
		t.Fatalf("create: exit %d %q", code, errs)
	}
	f.apiFails = true
	code, _, errs, _ := guardRun(t, f, DefaultPolicy(), "pr", "ready", "1")
	if code != 2 || !strings.Contains(errs, "502") || !f.pr(t, 0).Draft {
		t.Fatalf("ready on an API error: exit %d %q", code, errs)
	}
	g := newFake()
	g.setRules(repo, "main", tipV1, noRule)
	g.apiFails = true
	code, _, errs, _ = guardRun(t, g, DefaultPolicy(), "pr", "create", "--title", "feat: x")
	if code != 2 || len(g.prs) != 0 || !strings.Contains(errs, "retry") {
		t.Fatalf("create on an API error: exit %d %q prs=%d", code, errs, len(g.prs))
	}
}

func TestUnprotectedBaseIgnoresTheFile(t *testing.T) {
	f := newFake()
	f.setRules(repo, "main", tipV1, keyedRules)
	f.unprotected[repo+"/main"] = true
	code, _, errs, logs := guardRun(t, f, DefaultPolicy(), "pr", "create", "--title", "feat: x")
	if code != 0 {
		t.Fatalf("an unprotected base cannot impose rules: exit %d %q", code, errs)
	}
	if !strings.Contains(logs, "not protected") {
		t.Errorf("decision log must say why the file was ignored: %s", logs)
	}
	if f.callsWith("contents") != 0 {
		t.Errorf("read the file from an unprotected branch")
	}
}

func TestLockedPolicyRuleBeatsTheFile(t *testing.T) {
	f := newFake()
	f.setRules("tevo-studio/app", "main", tipV1, "version: 1\ntitle:\n  require: [\"CHE-\"]\n")
	f.defaultRepo = "tevo-studio/app"
	pol := DefaultPolicy()
	pol.Layers = []Layer{{Scope: ScopeOrg, Org: "tevo-studio", Locked: true, Rules: &Rules{Title: TitleRules{Forbid: []string{"CHE-"}}}}}

	code, _, errs, _ := guardRun(t, f, pol, "pr", "create", "--title", "feat: x (CHE-1)")
	if code != 2 || !strings.Contains(errs, "CHE-") {
		t.Fatalf("the locked forbid must refuse a CHE- title: exit %d %q", code, errs)
	}
	if code, _, errs, _ = guardRun(t, f, pol, "pr", "create", "--title", "feat: x"); code != 0 {
		t.Fatalf("the file's require must be ignored: exit %d %q", code, errs)
	}
}

func TestRulesLayerPrecedence(t *testing.T) {
	pol := DefaultPolicy()
	pol.Rules = &Rules{Trailer: "Refs:"}
	pol.Layers = []Layer{{Scope: ScopeRepo, Repo: repo, Rules: &Rules{Trailer: "Issue:"}}}
	file := &Rules{Trailer: "Closes:", Title: TitleRules{Pattern: "^feat"}}

	got := pol.resolve(Target{Repo: repo}, file, nil).Rules
	if got == nil || got.Trailer != "Issue:" || got.Title.Pattern != "^feat" {
		t.Errorf("repo layer beats workspace beats file, key by key: %+v", got)
	}
	pol.Locked = true
	got = pol.resolve(Target{Repo: repo}, file, nil).Rules
	if got == nil || got.Trailer != "Refs:" || got.Title.Pattern != "^feat" {
		t.Errorf("workspace lock freezes its keys only: %+v", got)
	}
}

func TestCheckPRIsReadOnly(t *testing.T) {
	f := newFake()
	f.setRules(repo, "main", tipV1, keyedRules)
	f.prs = append(f.prs, &fakePR{Number: 1, Repo: repo, HeadOwner: "cheese-work", HeadRef: "feature/x", Base: "main",
		Title: "feat: x [squash]", Draft: true})
	var out, errb bytes.Buffer
	e := &Engine{Policy: DefaultPolicy(), Runner: f, Out: &out, Err: &errb}
	code, err := e.CheckPR(context.Background(), []string{"1"})
	if err != nil || code != 1 {
		t.Fatalf("exit %d err %v: a PR without CHE-, a trailer or reviewer alice must fail", code, err)
	}
	if !strings.Contains(errb.String(), "CHE-") {
		t.Errorf("stderr %q", errb.String())
	}
	if n := f.callsWith("pr edit"); n != 0 {
		t.Errorf("CheckPR edited the PR %d times", n)
	}

	p := f.pr(t, 0)
	p.Title, p.Body, p.Reviewers = "feat: x (CHE-1) [squash]", "Refs: CHE-1", []string{"alice"}
	errb.Reset()
	if code, err = e.CheckPR(context.Background(), []string{"1"}); err != nil || code != 0 {
		t.Fatalf("a compliant PR: exit %d err %v %q", code, err, errb.String())
	}
}

func TestParsePolicyChecksRules(t *testing.T) {
	ok := `{"version":1,"plugins":[],"rules":{"trailer":"Refs:"},"layers":[{"scope":"org","org":"tevo-studio","locked":true,"rules":{"title":{"forbid":["CHE-"]}}}]}`
	if _, err := ParsePolicy([]byte(ok)); err != nil {
		t.Fatalf("valid rules: %v", err)
	}
	for name, doc := range map[string]string{
		"workspace bad regexp": `{"version":1,"plugins":[],"rules":{"title":{"pattern":"("}}}`,
		"layer both ways":      `{"version":1,"plugins":[],"layers":[{"scope":"org","org":"a","rules":{"title":{"require":["X"],"forbid":["X"]}}}]}`,
		"layer bad trailer":    `{"version":1,"plugins":[],"layers":[{"scope":"org","org":"a","rules":{"trailer":"Refs"}}]}`,
		"unknown rules key":    `{"version":1,"plugins":[],"rules":{"script":"x"}}`,
	} {
		if _, err := ParsePolicy([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCheckPRRefusesWithExit2OnUnreadableRules(t *testing.T) {
	pr := &fakePR{Number: 1, Repo: repo, HeadOwner: "cheese-work", HeadRef: "feature/x", Base: "main", Title: "feat: x [squash]", Draft: true}
	cases := map[string]func(f *fakeEnv){
		"malformed file": func(f *fakeEnv) { f.setRules(repo, "main", tipV1, "version: 1\nscript: x\n") },
		"API error":      func(f *fakeEnv) { f.setRules(repo, "main", tipV1, noRule); f.apiFails = true },
	}
	for name, setup := range cases {
		f := newFake()
		cp := *pr
		f.prs = append(f.prs, &cp)
		setup(f)
		var out, errb bytes.Buffer
		e := &Engine{Policy: DefaultPolicy(), Runner: f, Out: &out, Err: &errb}
		code, err := e.CheckPR(context.Background(), []string{"1"})
		if err != nil || code != 2 {
			t.Errorf("%s: exit %d err %v %q, want 2", name, code, err, errb.String())
		}
	}
}

func TestCheckPRDoesNotSayARequestFailed(t *testing.T) {
	f := newFake()
	f.setRules(repo, "main", tipV1, "version: 1\nreviewers: [alice]\n")
	f.prs = append(f.prs, &fakePR{Number: 1, Repo: repo, HeadOwner: "cheese-work", HeadRef: "feature/x", Base: "main", Title: "feat: x [squash]", Draft: true})
	var out, errb bytes.Buffer
	e := &Engine{Policy: DefaultPolicy(), Runner: f, Out: &out, Err: &errb}
	if code, _ := e.CheckPR(context.Background(), []string{"1"}); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if strings.Contains(errb.String(), "request failed") || !strings.Contains(errb.String(), "alice") {
		t.Errorf("read-only mode made no request: %q", errb.String())
	}
}
