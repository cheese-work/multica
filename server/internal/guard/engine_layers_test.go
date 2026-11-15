package guard

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func ghPRDeny(msg string) Plugin {
	return Plugin{Name: "gh-pr", Match: Match{Tool: "gh", Subcommand: []string{"pr"}}, Action: ActionDeny, Message: msg}
}

// layered: the built-in plugins plus one layer per scope.
func layered(layers ...Layer) *Policy {
	p := DefaultPolicy()
	p.Layers = layers
	return p
}

func TestLayersApplyAtTheCallTarget(t *testing.T) {
	pol := layered(
		Layer{Scope: ScopeOrg, Org: "acme", Review: []ReviewRule{{Repo: "acme/*", Reviewers: []string{"org-rev"}}}},
		Layer{Scope: ScopeRepo, Repo: "acme/locked-down", Plugins: []Plugin{ghPRDeny("repo says no")}},
		Layer{Scope: ScopePath, Repo: "acme/app", Path: "apps/web", Plugins: []Plugin{ghPRDeny("path says no")}},
	)
	cases := []struct {
		name, repo, prefix, wantErr, wantRev string
		code                                 int
	}{
		{"org layer adds reviewers", "acme/app", "", "", "org-rev", 0},
		{"repo layer denies", "acme/locked-down", "", "repo says no", "", 2},
		{"path layer denies inside its directory", "acme/app", "apps/web/src/", "path says no", "", 2},
		{"path layer does not reach a sibling", "acme/app", "apps/api/", "", "org-rev", 0},
		{"other org gets no layer", "other/app", "", "", "", 0},
	}
	for _, c := range cases {
		f := newFake()
		f.defaultRepo, f.prefix = c.repo, c.prefix
		code, _, errs := run(t, f, pol, "pr", "create", "--title", "a: b")
		if code != c.code || !strings.Contains(errs, c.wantErr) {
			t.Errorf("%s: exit %d %q", c.name, code, errs)
			continue
		}
		if c.code == 0 {
			if got := strings.Join(f.pr(t, 0).Reviewers, ","); got != c.wantRev {
				t.Errorf("%s: reviewers %q, want %q", c.name, got, c.wantRev)
			}
		}
	}
}

func TestLayersRepoFlagSelectsTarget(t *testing.T) {
	pol := layered(Layer{Scope: ScopeRepo, Repo: "acme/locked-down", Plugins: []Plugin{ghPRDeny("repo says no")}})
	f := newFake()
	code, _, errs := run(t, f, pol, "pr", "create", "--repo", "acme/locked-down", "--title", "a: b")
	if code != 2 || !strings.Contains(errs, "repo says no") {
		t.Errorf("exit %d %q", code, errs)
	}
}

func TestLayersRefuseWhenTargetUnknown(t *testing.T) {
	pol := layered(Layer{Scope: ScopeRepo, Repo: "acme/app", Plugins: []Plugin{ghPRDeny("repo says no")}})
	f := newFake()
	f.repoViewFails = true
	code, _, errs := run(t, f, pol, "pr", "create", "--title", "a: b")
	if code != 2 || len(f.streamed) != 0 || len(f.prs) != 0 || !strings.Contains(errs, "repository") {
		t.Errorf("exit %d streamed %v %q", code, f.streamed, errs)
	}
	// With no layers the target is not needed and the call runs as before.
	f = newFake()
	f.repoViewFails = true
	if code, _, _ := run(t, f, DefaultPolicy(), "run", "list"); code != 0 {
		t.Errorf("unlayered policy refused an unguarded call: %d", code)
	}
}

func TestInvalidPolicyFailsClosed(t *testing.T) {
	p, _ := writePolicy(t, t.TempDir(), goodPolicy)
	env := map[string]string{"MULTICA_GUARD_POLICY": p, "MULTICA_GUARD_POLICY_SHA256": strings.Repeat("a", 64)}
	pol, warn := PolicyFromEnv(func(k string) string { return env[k] })
	if warn == "" {
		t.Fatal("no warning")
	}
	f := newFake()
	code, _, errs := run(t, f, pol, "pr", "create", "--title", "a: b")
	if code != 2 || len(f.prs) != 0 || !strings.Contains(errs, "policy") {
		t.Errorf("unverifiable policy let gh pr run: exit %d prs %d %q", code, len(f.prs), errs)
	}
}

func TestLayersPRURLSelectsItsRepository(t *testing.T) {
	pol := layered(Layer{Scope: ScopeRepo, Repo: "acme/b", Plugins: []Plugin{ghPRDeny("b says no")}})
	f := newFake()
	f.defaultRepo = "acme/a" // the clone is acme/a; the URL names acme/b
	code, _, errs := run(t, f, pol, "pr", "ready", "https://github.com/acme/b/pull/1")
	if code != 2 || !strings.Contains(errs, "b says no") || len(f.streamed) != 0 {
		t.Errorf("exit %d streamed %v %q", code, f.streamed, errs)
	}
	// A URL in a flag value is not a target.
	f = newFake()
	f.defaultRepo = "acme/a"
	code, _, _ = run(t, f, pol, "pr", "create", "--title", "a: b", "--body", "see https://github.com/acme/b/pull/1")
	if code != 0 {
		t.Errorf("body text selected a repository: exit %d", code)
	}
}

func TestLayersPathUnknownFailsClosed(t *testing.T) {
	pol := layered(Layer{Scope: ScopePath, Repo: "acme/app", Path: "apps/web", Plugins: []Plugin{ghPRDeny("path says no")}})
	f := newFake()
	f.defaultRepo, f.prefixFails = "acme/app", true
	code, _, errs := run(t, f, pol, "pr", "create", "--title", "a: b")
	if code != 2 || len(f.prs) != 0 || !strings.Contains(errs, "directory") {
		t.Errorf("git failure dropped the path layers: exit %d prs %d %q", code, len(f.prs), errs)
	}
	// The target repository is not the clone: its path cannot be known.
	f = newFake()
	f.defaultRepo = "acme/other"
	code, _, errs = run(t, f, pol, "pr", "create", "--repo", "acme/app", "--title", "a: b")
	if code != 2 || len(f.prs) != 0 || !strings.Contains(errs, "directory") {
		t.Errorf("foreign repo ran without its path: exit %d prs %d %q", code, len(f.prs), errs)
	}
	// No path layer for the target: the directory is not needed.
	f = newFake()
	f.defaultRepo, f.prefixFails = "acme/app", true
	if code, _, errs = run(t, f, layered(Layer{Scope: ScopeRepo, Repo: "acme/x", Plugins: []Plugin{ghPRDeny("x")}}), "pr", "create", "--title", "a: b"); code != 0 {
		t.Errorf("unneeded git call refused the call: exit %d %q", code, errs)
	}
}

func TestLockdownRefusesEveryGuardedTool(t *testing.T) {
	p, _ := writePolicy(t, t.TempDir(), goodPolicy)
	env := map[string]string{"MULTICA_GUARD_POLICY": p, "MULTICA_GUARD_POLICY_SHA256": strings.Repeat("a", 64)}
	pol, _ := PolicyFromEnv(func(k string) string { return env[k] })
	for _, tool := range []string{"git", "gh"} {
		f := newFake()
		var out, errb bytes.Buffer
		e := &Engine{Policy: pol, Runner: f, Out: &out, Err: &errb}
		code, err := e.Exec(context.Background(), tool, []string{"status"})
		if err != nil || code != 2 || len(f.streamed) != 0 || !strings.Contains(errb.String(), "policy") {
			t.Errorf("%s: exit %d err %v streamed %v %q", tool, code, err, f.streamed, errb.String())
		}
	}
}
