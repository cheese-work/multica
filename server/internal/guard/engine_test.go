package guard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAPIWritesToPullsAreRefused(t *testing.T) {
	refused := [][]string{
		{"api", "repos/o/r/pulls", "-X", "POST", "-f", "title=x"},
		{"api", "/repos/o/r/pulls/3", "--method", "PATCH"},
		{"api", "repos/o/r/issues/3/comments", "-f", "body=x"}, // -f implies POST
		{"api", "repos/o/r/git/refs", "--input", "x.json"},
		{"api", "-X", "DELETE", "repos/o/r/issues/3"},
	}
	for _, args := range refused {
		f := newFake()
		code, _, errs := run(t, f, DefaultPolicy(), args...)
		if code != 2 || len(f.streamed) != 0 {
			t.Errorf("%v: exit %d streamed %v", args, code, f.streamed)
		}
		if !strings.Contains(errs, "gh pr") {
			t.Errorf("%v: line %q should name the command to use", args, errs)
		}
	}
	for _, args := range [][]string{
		{"api", "repos/o/r/pulls/3"},
		{"api", "repos/o/r/issues", "-X", "GET"},
		{"api", "repos/o/r/actions/runs", "-f", "x=y"},
		{"api", "user", "-X", "POST"},
	} {
		f := newFake()
		code, _, errs := run(t, f, DefaultPolicy(), args...)
		if code != 0 || len(f.streamed) != 1 || errs != "" {
			t.Errorf("%v: exit %d streamed %v errs %q", args, code, f.streamed, errs)
		}
	}
}

func TestAliasToGuardedCommandIsChecked(t *testing.T) {
	f := newFake()
	f.aliases["mk"] = "pr create --title"
	code, _, errs := run(t, f, DefaultPolicy(), "mk", "feat: via alias")
	if code != 0 {
		t.Fatalf("exit %d %q", code, errs)
	}
	if len(f.prs) != 1 || !f.pr(t, 0).Draft || !strings.HasSuffix(f.pr(t, 0).Title, "[squash]") {
		t.Errorf("alias bypassed the guard: %+v", f.prs)
	}
}

func TestShellAliasPassesThrough(t *testing.T) {
	f := newFake()
	f.aliases["sh"] = "!echo hi"
	if code, _, _ := run(t, f, DefaultPolicy(), "sh"); code != 0 || len(f.streamed) != 1 {
		t.Errorf("exit %d streamed %v", code, f.streamed)
	}
}

func TestPluginErrorFailsOpenWithOneWarningAndIsLogged(t *testing.T) {
	f := newFake()
	f.viewFails = true
	f.prs = []*fakePR{{Number: 7, Repo: "cheese-work/multica", HeadRef: "feature/x", Base: "main", Title: "feat: a", Draft: true}}
	var log bytes.Buffer
	var out, errb bytes.Buffer
	e := &Engine{Policy: DefaultPolicy(), Runner: f, Log: &log, Out: &out, Err: &errb}
	code, err := e.Exec(context.Background(), "gh", []string{"pr", "edit", "7", "--title", "feat: b"})
	if err != nil || code != 0 {
		t.Fatalf("exit %d err %v", code, err)
	}
	if len(f.streamed) != 1 {
		t.Errorf("original call must run once: %v", f.streamed)
	}
	if n := len(guardLines(errb.String())); n != 1 {
		t.Errorf("want one warning line, got %q", errb.String())
	}
	if !strings.Contains(log.String(), `"action":"plugin-error"`) {
		t.Errorf("log %q", log.String())
	}
}

func TestFailClosedPluginErrorRefuses(t *testing.T) {
	f := newFake()
	f.repoViewFails = true
	pol := DefaultPolicy()
	for i := range pol.Plugins {
		pol.Plugins[i].FailClosed = true
	}
	code, _, errs := run(t, f, pol, "pr", "create", "--title", "feat: a")
	if code != 2 || len(f.streamed) != 0 || len(f.prs) != 0 {
		t.Errorf("exit %d streamed %v prs %d %q", code, f.streamed, len(f.prs), errs)
	}
}

func TestDecisionLogHasNoArguments(t *testing.T) {
	f := newFake()
	var log bytes.Buffer
	var out, errb bytes.Buffer
	e := &Engine{Policy: policyWith(cheeseReview), Runner: f, Log: &log, Out: &out, Err: &errb}
	secret := "ghp_SECRET_TOKEN_VALUE"
	if _, err := e.Exec(context.Background(), "gh", []string{"pr", "create", "--title", "feat: a", "--body", secret}); err != nil {
		t.Fatal(err)
	}
	if log.Len() == 0 {
		t.Fatal("no decision logged")
	}
	if strings.Contains(log.String(), secret) {
		t.Errorf("log holds an argument: %q", log.String())
	}
	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(log.Bytes()), &rec); err != nil {
		t.Fatalf("log line is not json: %q: %v", log.String(), err)
	}
	for _, k := range []string{"plugin", "action", "tool", "line"} {
		if _, ok := rec[k]; !ok {
			t.Errorf("log lacks %q: %v", k, rec)
		}
	}
	if rec["action"] != "fixed" {
		t.Errorf("action %v", rec["action"])
	}
}

func writePolicy(t *testing.T, dir, body string) (string, string) {
	t.Helper()
	p := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	return p, hex.EncodeToString(sum[:])
}

const goodPolicy = `{"version":1,"plugins":[{"name":"gh-pr","match":{"tool":"gh","subcommand":["pr"]},"action":"fix","builtin":"gh-pr","allow_flags":["--draft","--title"]}],` +
	`"review":[{"repo":"cheese-work/multica","reviewers":["cheese"]}]}`

func TestLoadPolicyChecksDigest(t *testing.T) {
	p, sum := writePolicy(t, t.TempDir(), goodPolicy)
	pol, err := LoadPolicy(p, sum)
	if err != nil || len(pol.Plugins) != 1 || len(pol.Review) != 1 {
		t.Fatalf("pol %+v err %v", pol, err)
	}
	if _, err := LoadPolicy(p, strings.Repeat("0", 64)); err == nil {
		t.Error("a policy with the wrong digest was accepted")
	}
	if _, err := LoadPolicy(p, ""); err == nil {
		t.Error("a policy without a digest was accepted")
	}
}

func TestLoadPolicyRejectsBadPlugins(t *testing.T) {
	dir := t.TempDir()
	bad := map[string]string{
		"flag outside the allowlist": `{"version":1,"plugins":[{"name":"x","match":{"tool":"gh"},"action":"fix","builtin":"gh-pr","allow_flags":["--web"]}]}`,
		"unknown builtin":            `{"version":1,"plugins":[{"name":"x","match":{"tool":"gh"},"action":"fix","builtin":"shell"}]}`,
		"script field":               `{"version":1,"plugins":[{"name":"x","match":{"tool":"gh"},"action":"deny","script":"rm -rf /"}]}`,
		"fix without builtin":        `{"version":1,"plugins":[{"name":"x","match":{"tool":"gh"},"action":"fix"}]}`,
		"unknown action":             `{"version":1,"plugins":[{"name":"x","match":{"tool":"gh"},"action":"allow"}]}`,
		"bad github mode":            `{"version":1,"plugins":[],"review":[{"repo":"a/b","github":"maybe"}]}`,
		"unknown version":            `{"version":9,"plugins":[]}`,
	}
	for name, body := range bad {
		p, sum := writePolicy(t, dir, body)
		if _, err := LoadPolicy(p, sum); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPolicyFromEnvIgnoresTamperedCacheAndRepoFiles(t *testing.T) {
	dir := t.TempDir()
	p, _ := writePolicy(t, dir, goodPolicy)
	env := map[string]string{"MULTICA_GUARD_POLICY": p, "MULTICA_GUARD_POLICY_SHA256": strings.Repeat("a", 64)}
	pol, warn := PolicyFromEnv(func(k string) string { return env[k] })
	if warn == "" {
		t.Error("no warning for a policy that does not match its digest")
	}
	if len(pol.Review) != 0 {
		t.Errorf("tampered policy was used: %+v", pol.Review)
	}
	if len(pol.Plugins) == 0 {
		t.Error("fallback must keep the built-in plugins")
	}

	// A policy file in the working tree is never read.
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, ".multica"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"guard.json", "guard.yaml", "guard.yml"} {
		if err := os.WriteFile(filepath.Join(work, ".multica", name), []byte(goodPolicy), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(work)
	pol, warn = PolicyFromEnv(func(string) string { return "" })
	if warn != "" || len(pol.Review) != 0 {
		t.Errorf("repo policy file was read: warn %q review %+v", warn, pol.Review)
	}
}

func TestPolicyFromEnvUsesVerifiedPolicy(t *testing.T) {
	p, sum := writePolicy(t, t.TempDir(), goodPolicy)
	env := map[string]string{"MULTICA_GUARD_POLICY": p, "MULTICA_GUARD_POLICY_SHA256": sum}
	pol, warn := PolicyFromEnv(func(k string) string { return env[k] })
	if warn != "" || len(pol.Review) != 1 {
		t.Errorf("warn %q pol %+v", warn, pol)
	}
}

func TestReviewRuleMostSpecificWins(t *testing.T) {
	pol := policyWith(
		ReviewRule{Repo: "*", Reviewers: []string{"any"}},
		ReviewRule{Repo: "cheese-work/*", Reviewers: []string{"org"}},
		ReviewRule{Repo: "cheese-work/multica", Reviewers: []string{"exact"}},
	)
	for repo, want := range map[string]string{"cheese-work/multica": "exact", "cheese-work/other": "org", "x/y": "any"} {
		f := newFake()
		f.defaultRepo = repo
		if code, _, errs := run(t, f, pol, "pr", "create", "--title", "a: b"); code != 0 {
			t.Fatalf("%s: %d %q", repo, code, errs)
		}
		if got := strings.Join(f.pr(t, 0).Reviewers, ","); got != want {
			t.Errorf("%s: reviewers %q, want %q", repo, got, want)
		}
	}
}
