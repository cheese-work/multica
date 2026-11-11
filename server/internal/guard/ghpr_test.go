package guard

import (
	"strings"
	"sync"
	"testing"
)

var cheeseReview = ReviewRule{Repo: "cheese-work/multica", Reviewers: []string{"cheese", "x99-codex-terra"}}

func guardLines(stderr string) []string {
	var out []string
	for _, l := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(l, "guard:") {
			out = append(out, l)
		}
	}
	return out
}

func TestCreateForcesDraftTagAndReviewers(t *testing.T) {
	f := newFake()
	code, out, errs := run(t, f, policyWith(cheeseReview), "pr", "create", "--title", "feat(x): add y (CHE-1)", "--body", "b")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	if len(f.prs) != 1 {
		t.Fatalf("want 1 PR, got %d", len(f.prs))
	}
	p := f.pr(t, 0)
	if !p.Draft {
		t.Error("PR is not a draft")
	}
	if p.Title != "feat(x): add y (CHE-1) [squash]" {
		t.Errorf("title %q", p.Title)
	}
	if strings.Join(p.Reviewers, ",") != "cheese,x99-codex-terra" {
		t.Errorf("reviewers %v", p.Reviewers)
	}
	if !strings.Contains(out, p.url()) {
		t.Errorf("stdout %q lacks the PR url", out)
	}
	if n := len(guardLines(errs)); n != 1 {
		t.Errorf("want exactly one guard line, got %d: %q", n, errs)
	}
}

func TestCreateMergeTagByBranch(t *testing.T) {
	cases := []struct {
		branch  string
		merges  bool
		wantTag string
	}{
		{"sync/upstream-1", false, "[merge]"},
		{"merge/release", false, "[merge]"},
		{"upstream/v2", false, "[merge]"},
		{"feature/x", true, "[merge]"}, // tip is a merge of another line into base
		{"feature/x", false, "[squash]"},
	}
	for _, c := range cases {
		f := newFake()
		f.branch = c.branch
		f.merges[c.branch] = c.merges
		if code, _, errs := run(t, f, DefaultPolicy(), "pr", "create", "--title", "chore: t"); code != 0 {
			t.Fatalf("%s: exit %d %q", c.branch, code, errs)
		}
		if got := f.pr(t, 0).Title; !strings.HasSuffix(got, " "+c.wantTag) {
			t.Errorf("branch %s merges=%v: title %q, want tag %s", c.branch, c.merges, got, c.wantTag)
		}
	}
}

func TestCreateKeepsExistingTag(t *testing.T) {
	f := newFake()
	run(t, f, DefaultPolicy(), "pr", "create", "--title", "chore: t [merge]")
	if got := f.pr(t, 0).Title; got != "chore: t [merge]" {
		t.Errorf("title %q", got)
	}
}

func TestCreateRefusals(t *testing.T) {
	cases := map[string][]string{
		"fill":        {"pr", "create", "--fill"},
		"fill-first":  {"pr", "create", "--fill-first", "--title", "x"},
		"fill-verb":   {"pr", "create", "--fill-verbose"},
		"web":         {"pr", "create", "--web", "--title", "x"},
		"no title":    {"pr", "create", "--body", "b"},
		"empty title": {"pr", "create", "--title", ""},
		"draft false": {"pr", "create", "--title", "x", "--draft=false"},
	}
	for name, args := range cases {
		f := newFake()
		code, _, errs := run(t, f, DefaultPolicy(), args...)
		if code != 2 {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
		if len(f.prs) != 0 {
			t.Errorf("%s: a PR was created", name)
		}
		if n := len(guardLines(errs)); n != 1 {
			t.Errorf("%s: want one guard line, got %q", name, errs)
		}
	}
}

func TestCreateShortFlags(t *testing.T) {
	f := newFake()
	code, _, errs := run(t, f, policyWith(cheeseReview), "pr", "create", "-t", "fix: a", "-b", "b", "-r", "cheese")
	if code != 0 {
		t.Fatalf("exit %d %q", code, errs)
	}
	p := f.pr(t, 0)
	if p.Title != "fix: a [squash]" || !p.Draft {
		t.Errorf("title %q draft %v", p.Title, p.Draft)
	}
	if strings.Join(p.Reviewers, ",") != "cheese,x99-codex-terra" {
		t.Errorf("reviewers %v: agent's own reviewer stays, missing policy reviewer is added", p.Reviewers)
	}
}

func TestCreateRetryMakesOnePR(t *testing.T) {
	f := newFake()
	for i := 0; i < 3; i++ {
		code, out, errs := run(t, f, policyWith(cheeseReview), "pr", "create", "--title", "feat: a")
		if code != 0 {
			t.Fatalf("try %d: exit %d %q", i, code, errs)
		}
		if !strings.Contains(out, "/pull/1") {
			t.Errorf("try %d: stdout %q lacks the url", i, out)
		}
	}
	if len(f.prs) != 1 {
		t.Fatalf("want 1 PR after 3 tries, got %d", len(f.prs))
	}
	if f.callsWith("pr create") != 1 {
		t.Errorf("gh pr create ran %d times, want 1", f.callsWith("pr create"))
	}
}

func TestCreateConcurrentRetriesMakeOnePR(t *testing.T) {
	f := newFake()
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _, _ = run(t, f, policyWith(cheeseReview), "pr", "create", "--title", "feat: a")
		}()
	}
	wg.Wait()
	for i, c := range codes {
		if c != 0 {
			t.Errorf("caller %d exit %d", i, c)
		}
	}
	if len(f.prs) != 1 {
		t.Fatalf("want 1 PR, got %d", len(f.prs))
	}
	p := f.pr(t, 0)
	if !p.Draft || !strings.HasSuffix(p.Title, "[squash]") || len(p.Reviewers) != 2 {
		t.Errorf("PR not repaired: %+v", p)
	}
}

func TestCreateReviewerFailureAfterCreateIsRepaired(t *testing.T) {
	f := newFake()
	f.createReviewerFails = true
	code, out, errs := run(t, f, policyWith(cheeseReview), "pr", "create", "--title", "feat: a")
	if code != 0 {
		t.Fatalf("exit %d %q", code, errs)
	}
	if len(f.prs) != 1 || len(f.pr(t, 0).Reviewers) != 2 || !f.pr(t, 0).Draft {
		t.Errorf("not repaired: %+v", f.prs)
	}
	if !strings.Contains(out, "/pull/1") {
		t.Errorf("stdout %q", out)
	}
}

func TestCreateReviewerFailureUnrepairableStaysDraftAndFails(t *testing.T) {
	f := newFake()
	f.createReviewerFails = true
	f.editReviewerFails = true
	code, out, errs := run(t, f, policyWith(cheeseReview), "pr", "create", "--title", "feat: a")
	if code == 0 {
		t.Fatal("exit 0 on a PR without its reviewers")
	}
	if len(f.prs) != 1 || !f.pr(t, 0).Draft {
		t.Fatalf("PR must exist and stay draft: %+v", f.prs)
	}
	line := strings.Join(guardLines(errs), "\n")
	if !strings.Contains(line, f.pr(t, 0).url()) || !strings.Contains(line, "reviewer") {
		t.Errorf("guard line %q must name the PR url and the missing reviewers", line)
	}
	if strings.Contains(out, "Created") || strings.Contains(line, "ok") {
		t.Errorf("a success line was printed: %q / %q", out, line)
	}
}

func TestCreateReadbackErrorIsNotSuccess(t *testing.T) {
	f := newFake()
	f.viewFails = true
	code, _, errs := run(t, f, policyWith(cheeseReview), "pr", "create", "--title", "feat: a")
	if code == 0 {
		t.Fatal("exit 0 although the readback failed")
	}
	if len(f.prs) != 1 || !f.pr(t, 0).Draft {
		t.Fatalf("PR must exist and stay draft: %+v", f.prs)
	}
	if !strings.Contains(errs, f.pr(t, 0).url()) {
		t.Errorf("stderr %q must name the PR url", errs)
	}
}

func TestCreateRepoFlagTargetsThatRepo(t *testing.T) {
	f := newFake()
	f.defaultRepo = "cheese-work/multica"
	code, _, errs := run(t, f, DefaultPolicy(), "pr", "create", "--title", "feat: a", "--repo", "other/repo")
	if code != 0 {
		t.Fatalf("a --repo target is a valid target: exit %d %q", code, errs)
	}
	if f.pr(t, 0).Repo != "other/repo" {
		t.Errorf("repo %q", f.pr(t, 0).Repo)
	}
}

func TestForkBranchNamesDoNotCollide(t *testing.T) {
	f := newFake()
	// someone else's fork already has an open PR from a branch with the same name
	f.prs = append(f.prs, &fakePR{Number: 1, Repo: "cheese-work/multica", HeadOwner: "stranger", HeadRef: "feature/x",
		Base: "main", Title: "theirs [squash]", Draft: true})
	f.remoteOwner = "congvc-bot"
	code, out, errs := run(t, f, policyWith(cheeseReview), "pr", "create", "--title", "feat: mine")
	if code != 0 {
		t.Fatalf("exit %d %q", code, errs)
	}
	if len(f.prs) != 2 {
		t.Fatalf("want a second PR for my fork, got %d", len(f.prs))
	}
	if f.pr(t, 0).Title != "theirs [squash]" || len(f.pr(t, 0).Reviewers) != 0 {
		t.Errorf("the stranger's PR was touched: %+v", f.pr(t, 0))
	}
	if !strings.Contains(out, "/pull/2") {
		t.Errorf("stdout %q", out)
	}
	// retrying finds my PR, not theirs
	code, out, _ = run(t, f, policyWith(cheeseReview), "pr", "create", "--title", "feat: mine")
	if code != 0 || len(f.prs) != 2 || !strings.Contains(out, "/pull/2") {
		t.Errorf("retry: exit %d, %d PRs, out %q", code, len(f.prs), out)
	}
}

func TestForkHeadFlagPicksTheOwner(t *testing.T) {
	f := newFake()
	f.remoteOwner = ""
	f.prs = append(f.prs, &fakePR{Number: 1, Repo: "cheese-work/multica", HeadOwner: "stranger", HeadRef: "fix", Base: "main", Title: "t [squash]", Draft: true})
	code, out, errs := run(t, f, DefaultPolicy(), "pr", "create", "--title", "fix: z", "--head", "congvc-bot:fix")
	if code != 0 {
		t.Fatalf("exit %d %q", code, errs)
	}
	if len(f.prs) != 2 || !strings.Contains(out, "/pull/2") {
		t.Errorf("%d PRs, out %q", len(f.prs), out)
	}
}

func TestUnknownHeadOwnerWithSameNamedPRIsRefused(t *testing.T) {
	f := newFake()
	f.remoteOwner = "" // the guard cannot tell whose branch this is
	f.prs = append(f.prs, &fakePR{Number: 1, Repo: "cheese-work/multica", HeadOwner: "stranger", HeadRef: "feature/x", Base: "main", Title: "t [squash]", Draft: true})
	code, _, errs := run(t, f, DefaultPolicy(), "pr", "create", "--title", "fix: z")
	if code != 2 || len(f.prs) != 1 {
		t.Fatalf("exit %d, %d PRs; want refusal and no second PR", code, len(f.prs))
	}
	if !strings.Contains(errs, "--head") {
		t.Errorf("line %q should tell the agent to pass --head owner:branch", errs)
	}
}

func TestSkipRuleRequestsNoReviewer(t *testing.T) {
	f := newFake()
	f.defaultRepo = "upstream/repo"
	pol := policyWith(ReviewRule{Repo: "upstream/*", GitHub: "skip", Reviewers: []string{"cheese"}})
	code, _, errs := run(t, f, pol, "pr", "create", "--title", "fix: u")
	if code != 0 || len(f.pr(t, 0).Reviewers) != 0 {
		t.Fatalf("exit %d reviewers %v %q", code, f.pr(t, 0).Reviewers, errs)
	}
}

func TestEditTitleGetsTag(t *testing.T) {
	f := newFake()
	f.prs = []*fakePR{{Number: 7, Repo: "cheese-work/multica", HeadRef: "sync/up", Base: "main", Title: "old [squash]", Draft: true}}
	code, _, errs := run(t, f, DefaultPolicy(), "pr", "edit", "7", "--title", "chore: sync")
	if code != 0 {
		t.Fatalf("exit %d %q", code, errs)
	}
	if got := f.pr(t, 0).Title; got != "chore: sync [merge]" {
		t.Errorf("title %q", got)
	}
	if n := len(guardLines(errs)); n != 1 {
		t.Errorf("want one guard line, got %q", errs)
	}
}

func TestEditWithoutTitleOrWithTagPassesUntouched(t *testing.T) {
	for _, args := range [][]string{
		{"pr", "edit", "7", "--body", "x"},
		{"pr", "edit", "7", "--title", "ok [squash]"},
	} {
		f := newFake()
		f.prs = []*fakePR{{Number: 7, Repo: "cheese-work/multica", HeadRef: "f", Base: "main", Title: "t"}}
		code, _, errs := run(t, f, DefaultPolicy(), args...)
		if code != 0 || len(guardLines(errs)) != 0 {
			t.Errorf("%v: exit %d %q", args, code, errs)
		}
		if len(f.streamed) != 1 || f.streamed[0] != "gh "+strings.Join(args, " ") {
			t.Errorf("%v: streamed %v", args, f.streamed)
		}
	}
}

func TestReadyRepairsThenMarksReady(t *testing.T) {
	f := newFake()
	f.prs = []*fakePR{{Number: 7, Repo: "cheese-work/multica", HeadRef: "feature/x", Base: "main", Title: "feat: a", Draft: true}}
	code, _, errs := run(t, f, policyWith(cheeseReview), "pr", "ready", "7")
	if code != 0 {
		t.Fatalf("exit %d %q", code, errs)
	}
	p := f.pr(t, 0)
	if p.Draft {
		t.Error("PR still draft")
	}
	if p.Title != "feat: a [squash]" || len(p.Reviewers) != 2 {
		t.Errorf("not repaired: %+v", p)
	}
}

func TestReadyRefusedWhenReviewerCannotBeRequested(t *testing.T) {
	f := newFake()
	f.editReviewerFails = true
	f.prs = []*fakePR{{Number: 7, Repo: "cheese-work/multica", HeadRef: "feature/x", Base: "main", Title: "feat: a [squash]", Draft: true}}
	code, _, errs := run(t, f, policyWith(cheeseReview), "pr", "ready", "7")
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !f.pr(t, 0).Draft {
		t.Error("PR must stay draft")
	}
	for _, s := range f.streamed {
		if strings.Contains(s, "pr ready") {
			t.Errorf("gh pr ready ran: %v", f.streamed)
		}
	}
	if !strings.Contains(errs, "reviewer") {
		t.Errorf("line %q must name what is missing", errs)
	}
}

func TestReadyUndoPassesThrough(t *testing.T) {
	f := newFake()
	f.prs = []*fakePR{{Number: 7, Repo: "cheese-work/multica", HeadRef: "f", Base: "main", Title: "t"}}
	code, _, errs := run(t, f, policyWith(cheeseReview), "pr", "ready", "7", "--undo")
	if code != 0 || len(guardLines(errs)) != 0 || f.callsWith("pr edit") != 0 {
		t.Errorf("exit %d, calls %v, errs %q", code, f.calls, errs)
	}
}

func TestStreamedExitCodeIsKept(t *testing.T) {
	f := newFake()
	f.streamExit = 4
	code, _, _ := run(t, f, DefaultPolicy(), "issue", "list")
	if code != 4 {
		t.Errorf("exit %d, want 4", code)
	}
}

// Preflight and readback failures must not let an unguarded call through:
// that would open a non-draft PR or mark one ready without the checks.
func TestCreateRefusedWhenPreflightFails(t *testing.T) {
	cases := map[string]func(*fakeEnv){
		"repo view fails": func(f *fakeEnv) { f.repoViewFails = true },
		"pr list fails":   func(f *fakeEnv) { f.listFails = true },
		"branch unknown":  func(f *fakeEnv) { f.branch = "" },
	}
	for name, inject := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake()
			inject(f)
			code, _, errs := run(t, f, policyWith(cheeseReview), "pr", "create", "--title", "feat: a")
			if code != 2 {
				t.Fatalf("exit %d, want 2 (%q)", code, errs)
			}
			if len(f.streamed) != 0 || len(f.prs) != 0 {
				t.Errorf("an unguarded create ran: streamed %v prs %d", f.streamed, len(f.prs))
			}
			if n := len(guardLines(errs)); n != 1 {
				t.Errorf("want one guard line, got %q", errs)
			}
		})
	}
}

func TestReadyRefusedWhenViewFails(t *testing.T) {
	f := newFake()
	f.viewFails = true
	f.prs = []*fakePR{{Number: 7, Repo: "cheese-work/multica", HeadRef: "feature/x", Base: "main", Title: "feat: a [squash]", Draft: true}}
	code, _, errs := run(t, f, policyWith(cheeseReview), "pr", "ready", "7")
	if code != 2 {
		t.Fatalf("exit %d, want 2 (%q)", code, errs)
	}
	if !f.pr(t, 0).Draft {
		t.Error("PR must stay draft")
	}
	if len(f.streamed) != 0 {
		t.Errorf("gh pr ready ran: %v", f.streamed)
	}
}

func TestPreflightFailureStillPassesWhenVerifyIsOff(t *testing.T) {
	f := newFake()
	f.viewFails = true
	f.prs = []*fakePR{{Number: 7, Repo: "cheese-work/multica", HeadRef: "feature/x", Base: "main", Title: "feat: a", Draft: true}}
	off := false
	pol := DefaultPolicy()
	pol.Verify = &off
	if code, _, _ := run(t, f, pol, "pr", "ready", "7"); code != 0 || len(f.streamed) != 1 {
		t.Errorf("exit %d streamed %v", code, f.streamed)
	}
}
