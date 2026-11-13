package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// runTool is run() for any tool, returning the decision log as well.
func runTool(t *testing.T, f *fakeEnv, tool string, args ...string) (code int, errs string, log string) {
	t.Helper()
	var out, errb, logb bytes.Buffer
	e := &Engine{Policy: DefaultPolicy(), Runner: f, Log: &logb, Out: &out, Err: &errb}
	code, err := e.Exec(context.Background(), tool, args)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	return code, errb.String(), logb.String()
}

// CI-wait calls are refused with the wakeup line, whatever the flag form, the
// position of -R, or the alias they hide behind (CHE-5206, plugin 2).
func TestCIWaitCallsAreDenied(t *testing.T) {
	f := newFake()
	f.aliases["cw"] = "pr checks $1 --watch"
	f.aliases["rw"] = "run watch"
	denied := map[string][]string{
		"pr checks --watch":         {"pr", "checks", "5", "--watch"},
		"pr checks --watch=true":    {"pr", "checks", "5", "--watch=true"},
		"pr checks -R before watch": {"pr", "checks", "-R", "o/r", "5", "--watch", "--interval", "5"},
		"pr -R checks":              {"pr", "-R", "o/r", "checks", "5", "--watch"},
		"pr checks no number":       {"pr", "checks", "--watch"},
		"run watch":                 {"run", "watch", "123"},
		"run watch no id":           {"run", "watch"},
		"run -R watch":              {"run", "-R", "o/r", "watch", "1", "--exit-status"},
		"alias to pr checks":        {"cw", "5"},
		"alias to run watch":        {"rw", "9"},
	}
	for name, args := range denied {
		t.Run(name, func(t *testing.T) {
			f.streamed = nil
			code, errs, log := runTool(t, f, "gh", args...)
			if code != 2 || len(f.streamed) != 0 {
				t.Fatalf("exit %d streamed %v, want a refusal that never reaches gh", code, f.streamed)
			}
			if !strings.HasPrefix(errs, "guard: ") || strings.Count(errs, "\n") != 1 || !strings.Contains(errs, "multica issue wakeup create") {
				t.Errorf("line %q must be one guard line with the wakeup command", errs)
			}
			var rec logRecord
			if err := json.Unmarshal([]byte(strings.TrimSpace(log)), &rec); err != nil || rec.Action != KindRefused || rec.Plugin != "ci-wait" {
				t.Errorf("decision log %q: %+v %v", log, rec, err)
			}
		})
	}
}

func TestCIWaitLineNamesTheMatchingCondition(t *testing.T) {
	f := newFake()
	_, errs, _ := runTool(t, f, "gh", "pr", "checks", "5", "--watch")
	if !strings.Contains(errs, "--until-pr checks") {
		t.Errorf("pr checks line %q should name --until-pr checks", errs)
	}
	_, errs, _ = runTool(t, f, "gh", "run", "watch", "7")
	if !strings.Contains(errs, "--until-gh-run") {
		t.Errorf("run watch line %q should name --until-gh-run", errs)
	}
}

// One read of the checks, a run listing or a finished-state API read is not a
// wait and passes untouched and silent.
func TestCIReadsPass(t *testing.T) {
	for _, args := range [][]string{
		{"pr", "checks", "5"},
		{"pr", "checks", "5", "--required"},
		{"pr", "checks", "5", "--watch=false"},
		{"pr", "checks", "5", "-w"},    // -w is --web, not --watch
		{"pr", "view", "5", "--watch"}, // not a checks call
		{"run", "view", "1"},
		{"run", "list", "-L", "5"},
		{"api", "repos/o/r/actions/runs/1"},
		{"api", "repos/o/r/commits/abc/check-runs"},
	} {
		f := newFake()
		code, errs, log := runTool(t, f, "gh", args...)
		if code != 0 || len(f.streamed) != 1 || errs != "" {
			t.Errorf("%v: exit %d streamed %v errs %q", args, code, f.streamed, errs)
		}
		if strings.Contains(log, `"ci-wait"`) {
			t.Errorf("%v: a pass must not be logged as a ci-wait decision: %s", args, log)
		}
	}
}

// Plugin 2 must not hide a call that plugin 1 still has to see.
func TestCIWaitPluginDoesNotShadowPRPlugin(t *testing.T) {
	f := newFake()
	code, _, errs := run(t, f, DefaultPolicy(), "pr", "create", "--title", "feat: x")
	if code != 0 || len(f.prs) != 1 || !f.pr(t, 0).Draft || !strings.HasSuffix(f.pr(t, 0).Title, "[squash]") {
		t.Fatalf("exit %d errs %q prs %+v", code, errs, f.prs)
	}
}

func TestSleepFromTwentySecondsIsDenied(t *testing.T) {
	for _, args := range [][]string{
		{"20"}, {"30"}, {"1m"}, {"2h"}, {"1d"}, {"infinity"}, {"10", "10"}, {"0.5", "20"}, {"--", "60"}, {"19.5", "0.5"},
	} {
		f := newFake()
		code, errs, _ := runTool(t, f, "sleep", args...)
		if code != 2 || len(f.streamed) != 0 || !strings.Contains(errs, "multica issue wakeup create") {
			t.Errorf("sleep %v: exit %d streamed %v line %q", args, code, f.streamed, errs)
		}
	}
	for _, args := range [][]string{{"1"}, {"5"}, {"19"}, {"19.9"}, {"0.2"}, {"10s"}, {"nonsense"}, {}} {
		f := newFake()
		code, errs, _ := runTool(t, f, "sleep", args...)
		if code != 0 || len(f.streamed) != 1 || errs != "" {
			t.Errorf("sleep %v: exit %d streamed %v errs %q", args, code, f.streamed, errs)
		}
	}
}

// A `gh api` path is no way around the guard: GraphQL mutations (which carry
// no repos/*/pulls path) and a request body the guard cannot read are refused;
// reads and plain GraphQL queries pass.
func TestGHAPIGraphQLMutationIsRefused(t *testing.T) {
	for _, args := range [][]string{
		{"api", "graphql", "-f", "query=mutation { markPullRequestReadyForReview(input:{pullRequestId:\"x\"}) { clientMutationId } }"},
		{"api", "graphql", "-F", "query=mutation($id:ID!){ createPullRequest(input:{}){ clientMutationId } }"},
		{"api", "graphql", "--raw-field", "query= Mutation { x }"},
		{"api", "graphql", "-F", "query=@mutation.graphql"},
		{"api", "graphql", "--input", "body.json"},
		{"api", "-X", "POST", "/graphql", "-f", "query=mutation{x}"},
	} {
		f := newFake()
		code, _, errs := run(t, f, DefaultPolicy(), args...)
		if code != 2 || len(f.streamed) != 0 || !strings.Contains(errs, "gh pr") {
			t.Errorf("%v: exit %d streamed %v errs %q", args, code, f.streamed, errs)
		}
	}
	for _, args := range [][]string{
		{"api", "graphql", "-f", "query=query { viewer { login } }"},
		{"api", "graphql", "-f", "query={ repository(owner:\"o\", name:\"r\") { pullRequest(number:1) { mergeStateStatus } } }"},
		{"api", "graphql", "-f", "query=query { viewer { login } }", "-f", "after=mutation"}, // another field, not the query
	} {
		f := newFake()
		code, _, errs := run(t, f, DefaultPolicy(), args...)
		if code != 0 || len(f.streamed) != 1 || errs != "" {
			t.Errorf("%v: exit %d streamed %v errs %q", args, code, f.streamed, errs)
		}
	}
}

// A shell alias runs its text through sh; the gh inside resolves on PATH to the
// shim and is checked there, so the guard leaves the alias itself alone.
func TestShellAliasIsLeftToTheShimOnReentry(t *testing.T) {
	f := newFake()
	f.aliases["sw"] = "!gh run watch $1"
	code, errs, _ := runTool(t, f, "gh", "sw", "1")
	if code != 0 || len(f.streamed) != 1 || errs != "" {
		t.Errorf("exit %d streamed %v errs %q", code, f.streamed, errs)
	}
}

// sleep is not a repository call: a policy that failed to load (lockdown) or
// that has layers must not refuse a short sleep or look up a repository for it.
func TestShortSleepIgnoresLockdownAndLayers(t *testing.T) {
	layered := DefaultPolicy()
	layered.Layers = []Layer{{Scope: ScopePath, Repo: "o/r", Path: "x"}}
	for name, pol := range map[string]*Policy{"lockdown": lockdownPolicy(), "layered": layered} {
		f := newFake()
		f.repoViewFails = true
		var out, errb bytes.Buffer
		e := &Engine{Policy: pol, Runner: f, Out: &out, Err: &errb}
		code, err := e.Exec(context.Background(), "sleep", []string{"5"})
		if err != nil || code != 0 || len(f.streamed) != 1 || errb.String() != "" || len(f.calls) != 0 {
			t.Errorf("%s: short sleep exit %d err %v streamed %v errs %q calls %v", name, code, err, f.streamed, errb.String(), f.calls)
		}
		f = newFake()
		e = &Engine{Policy: pol, Runner: f, Out: &out, Err: &errb}
		// A lockdown policy carries no plugins, so a long sleep runs too.
		want := 2
		if name == "lockdown" {
			want = 0
		}
		if code, _ = e.Exec(context.Background(), "sleep", []string{"60"}); code != want {
			t.Errorf("%s: sleep 60 exit %d, want %d", name, code, want)
		}
	}
}
