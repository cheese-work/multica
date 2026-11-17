package guard

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const queueRef = "refs/heads/gh-readonly-queue/main/pr-1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// queueEnv is the environment of a merge_group run of pr-guard-rules.yml.
func queueEnv(over map[string]string) func(string) string {
	m := map[string]string{
		"GUARD_RULES_VIA_GIT": "1", "GITHUB_EVENT_NAME": "merge_group", "MERGE_GROUP_HEAD_REF": queueRef,
		"GH_TOKEN": "s3cret-token",
	}
	for k, v := range over {
		m[k] = v
	}
	return func(k string) string { return m[k] }
}

func queuePR() *fakePR {
	return &fakePR{Number: 1, Repo: repo, HeadOwner: "cheese-work", HeadRef: "feature/x", Base: "main",
		Title: "feat: x [squash]", Draft: true}
}

func checkPR(t *testing.T, f *fakeEnv, env func(string) string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	e := &Engine{Policy: DefaultPolicy(), Runner: f, Out: &out, Err: &errb, Env: env}
	code, err := e.CheckPR(context.Background(), []string{"1"})
	if err != nil {
		t.Fatalf("CheckPR: %v", err)
	}
	return code, out.String(), errb.String()
}

// 1. Absent file, every REST call rate-limited: the Git path judges the PR.
func TestQueueReadsThroughGitWhenRESTIsRateLimited(t *testing.T) {
	f := newFake()
	f.prs = append(f.prs, queuePR())
	f.tips[repo+"/main"] = tipV1
	f.apiFails = true
	code, out, errs := checkPR(t, f, queueEnv(nil))
	if code != 0 || !strings.Contains(out, "guard: ok") {
		t.Fatalf("exit %d out %q err %q, want a judged, passing PR", code, out, errs)
	}
	if n := f.callsWith("api"); n != 0 {
		t.Errorf("%d REST calls: %v", n, f.calls)
	}
	if n := f.callsWith("s3cret-token"); n != 0 {
		t.Errorf("the token is on a command line: %v", f.calls)
	}
}

// 2. The base moved after checkout: the live tip's rule refuses the PR.
func TestQueueUsesTheLiveTip(t *testing.T) {
	f := newFake()
	f.prs = append(f.prs, queuePR())
	f.setRules(repo, "main", tipV2, "version: 1\ntitle:\n  require: [\"CHE-\"]\n")
	f.files[repo+"@"+tipV1] = noRule // the older tip would pass
	code, _, errs := checkPR(t, f, queueEnv(nil))
	if code != 1 || !strings.Contains(errs, "CHE-") {
		t.Fatalf("exit %d %q, want 1 on the rule at the live tip", code, errs)
	}
	// Same fixture through REST gives the same decision.
	g := newFake()
	g.prs = append(g.prs, queuePR())
	g.setRules(repo, "main", tipV2, "version: 1\ntitle:\n  require: [\"CHE-\"]\n")
	if rcode, _, rerrs := checkPR(t, g, nil); rcode != code || rerrs != errs {
		t.Errorf("REST gave exit %d %q, Git gave %d %q", rcode, rerrs, code, errs)
	}
}

// 3. Two tips, two rule sets, two decisions.
func TestQueueChangedRule(t *testing.T) {
	f := newFake()
	f.prs = append(f.prs, queuePR())
	f.setRules(repo, "main", tipV1, noRule)
	if code, _, errs := checkPR(t, f, queueEnv(nil)); code != 0 {
		t.Fatalf("tip 1: exit %d %q", code, errs)
	}
	f.setRules(repo, "main", tipV2, "version: 1\ntrailer: \"Refs:\"\n")
	if code, _, errs := checkPR(t, f, queueEnv(nil)); code != 1 || !strings.Contains(errs, "Refs:") {
		t.Fatalf("tip 2: exit %d %q", code, errs)
	}
}

// 4. A malformed file at the live tip is refused, as through REST.
func TestQueueMalformedFile(t *testing.T) {
	f := newFake()
	f.prs = append(f.prs, queuePR())
	f.setRules(repo, "main", tipV1, "version: 1\nscript: x\n")
	code, _, errs := checkPR(t, f, queueEnv(nil))
	if code != 2 || !strings.Contains(errs, "malformed") {
		t.Fatalf("exit %d %q, want 2 malformed", code, errs)
	}
}

// 5. A Git failure falls back to REST with today's call sequence and output.
func TestQueueGitFailureFallsBackToREST(t *testing.T) {
	f := newFake()
	f.prs = append(f.prs, queuePR())
	f.setRules(repo, "main", tipV1, keyedRules)
	f.gitFetchExit = 128
	code, out, _ := checkPR(t, f, queueEnv(nil))

	g := newFake()
	g.prs = append(g.prs, queuePR())
	g.setRules(repo, "main", tipV1, keyedRules)
	wcode, wout, _ := checkPR(t, g, nil)
	if code != wcode || out != wout {
		t.Errorf("exit %d %q, want today's %d %q", code, out, wcode, wout)
	}
	var rest []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "gh ") {
			rest = append(rest, c)
		}
	}
	if !reflect.DeepEqual(rest, g.calls) {
		t.Errorf("REST sequence\n%v\nwant\n%v", rest, g.calls)
	}

	// REST fails too: exit 2 as today.
	f.apiFails = true
	if code, _, errs := checkPR(t, f, queueEnv(nil)); code != 2 {
		t.Errorf("both failed: exit %d %q, want 2", code, errs)
	}
}

// 6. Context off: the call sequence equals today's.
func TestQueueContextOffKeepsTheRESTSequence(t *testing.T) {
	off := map[string]map[string]string{
		"no flag":           {"GUARD_RULES_VIA_GIT": ""},
		"pull_request":      {"GITHUB_EVENT_NAME": "pull_request"},
		"other base":        {"MERGE_GROUP_HEAD_REF": "refs/heads/gh-readonly-queue/release/pr-1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		"no queue ref":      {"MERGE_GROUP_HEAD_REF": ""},
		"slash base prefix": {"MERGE_GROUP_HEAD_REF": "refs/heads/gh-readonly-queue/x/main/pr-1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}
	g := newFake()
	g.prs = append(g.prs, queuePR())
	g.setRules(repo, "main", tipV1, keyedRules)
	checkPR(t, g, nil)
	for name, over := range off {
		f := newFake()
		f.prs = append(f.prs, queuePR())
		f.setRules(repo, "main", tipV1, keyedRules)
		checkPR(t, f, queueEnv(over))
		if !reflect.DeepEqual(f.calls, g.calls) {
			t.Errorf("%s: calls\n%v\nwant\n%v", name, f.calls, g.calls)
		}
	}
}

// 7. Parity on a real local Git repository: absent and present file.
func TestGitReaderOnARealRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	srv := t.TempDir()
	src := filepath.Join(srv, "o", "n.git")
	sh := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	sh(src, "init", "-q", "-b", "main")
	sh(src, "config", "uploadpack.allowFilter", "true")
	sh(src, "config", "uploadpack.allowAnySHA1InWant", "true")
	sh(src, "commit", "-q", "--allow-empty", "-m", "no rules")

	env := func(k string) string {
		return map[string]string{"GITHUB_SERVER_URL": "file://" + srv}[k]
	}
	read := func() (*Rules, []string, string) {
		e := &Engine{Policy: DefaultPolicy(), Runner: ExecRunner{}, Env: env}
		rules, revs, handled, err := e.fetchRulesViaGit(context.Background(), "o/n", "main")
		if err != nil || !handled {
			t.Fatalf("handled %v err %v", handled, err)
		}
		return rules, revs, e.rulesNote
	}
	rules, revs, note := read()
	if rules != nil || revs != nil || !strings.Contains(note, "no "+RepoRulesFile) {
		t.Fatalf("absent file: %+v %v %q", rules, revs, note)
	}

	if err := os.MkdirAll(filepath.Join(src, ".github"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, RepoRulesFile), []byte(keyedRules), 0o644); err != nil {
		t.Fatal(err)
	}
	sh(src, "add", ".")
	sh(src, "commit", "-q", "-m", "rules")
	rules, revs, note = read()
	want, wantRevs, _ := ParseRepoRules([]byte(keyedRules))
	if !reflect.DeepEqual(rules, want) || !reflect.DeepEqual(revs, wantRevs) {
		t.Fatalf("present file: %+v %v, want %+v %v", rules, revs, want, wantRevs)
	}
	if !strings.Contains(note, "repo rules: "+RepoRulesFile+" at base tip") {
		t.Errorf("note %q", note)
	}
}
