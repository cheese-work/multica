package guard

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pl(name, action, msg string) Plugin {
	return Plugin{Name: name, Match: Match{Tool: "gh", Subcommand: []string{name}}, Action: action, Message: msg}
}

func plug(p *Policy, name string) *Plugin {
	for i := range p.Plugins {
		if p.Plugins[i].Name == name {
			return &p.Plugins[i]
		}
	}
	return nil
}

// ladderDoc: workspace denies "x"; each deeper layer tries to turn it into a pass.
func ladderDoc() *Policy {
	return &Policy{
		Version: 1,
		Plugins: []Plugin{pl("x", ActionDeny, "ws")},
		Layers: []Layer{
			{Scope: ScopeOrg, Org: "acme", Plugins: []Plugin{pl("x", ActionDeny, "org")}},
			{Scope: ScopeRepo, Repo: "acme/app", Plugins: []Plugin{pl("x", ActionDeny, "repo")}},
			{Scope: ScopePath, Repo: "acme/app", Path: "apps/web", Plugins: []Plugin{pl("x", ActionDeny, "path")}},
		},
	}
}

func TestResolvePrecedencePerScope(t *testing.T) {
	cases := []struct {
		name string
		tgt  Target
		want string
	}{
		{"workspace only", Target{}, "ws"},
		{"other org stays workspace", Target{Repo: "other/app"}, "ws"},
		{"org overrides workspace", Target{Repo: "acme/lib"}, "org"},
		{"repo overrides org", Target{Repo: "acme/app"}, "repo"},
		{"path overrides repo", Target{Repo: "acme/app", Path: "apps/web/src"}, "path"},
		{"path prefix is a directory boundary", Target{Repo: "acme/app", Path: "apps/webby"}, "repo"},
		{"owner match ignores case", Target{Repo: "ACME/App", Path: "apps/web"}, "path"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := plug(ladderDoc().Resolve(c.tgt), "x")
			if got == nil || got.Message != c.want {
				t.Fatalf("got %+v, want message %q", got, c.want)
			}
		})
	}
}

func TestResolveLockedWinsOverMoreSpecific(t *testing.T) {
	for _, lockAt := range []string{"workspace", "org", "repo"} {
		t.Run(lockAt, func(t *testing.T) {
			d := ladderDoc()
			switch lockAt {
			case "workspace":
				d.Locked = true
			case "org":
				d.Layers[0].Locked = true
			case "repo":
				d.Layers[1].Locked = true
			}
			want := map[string]string{"workspace": "ws", "org": "org", "repo": "repo"}[lockAt]
			got := plug(d.Resolve(Target{Repo: "acme/app", Path: "apps/web"}), "x")
			if got == nil || got.Message != want {
				t.Fatalf("locked at %s: got %+v, want %q", lockAt, got, want)
			}
		})
	}
}

func TestResolveLockedFirstLockWins(t *testing.T) {
	d := ladderDoc()
	d.Layers[0].Locked = true // org
	d.Layers[1].Locked = true // repo, deeper: must not displace the org lock
	got := plug(d.Resolve(Target{Repo: "acme/app"}), "x")
	if got == nil || got.Message != "org" {
		t.Fatalf("got %+v, want org", got)
	}
}

func TestResolveLockIsPerKeyAndDoesNotBlockOthers(t *testing.T) {
	d := &Policy{
		Version: 1, Locked: true,
		Plugins: []Plugin{pl("x", ActionDeny, "ws")},
		Layers: []Layer{{Scope: ScopeOrg, Org: "acme",
			Plugins: []Plugin{pl("x", ActionPass, ""), pl("y", ActionDeny, "org-y")}}},
	}
	got := d.Resolve(Target{Repo: "acme/app"})
	if x := plug(got, "x"); x == nil || x.Action != ActionDeny {
		t.Fatalf("locked x was overridden: %+v", x)
	}
	if y := plug(got, "y"); y == nil || y.Message != "org-y" {
		t.Fatalf("new key y from deeper layer was dropped: %+v", y)
	}
}

func TestResolveVerifyAndReviewKeys(t *testing.T) {
	off, on := false, true
	d := &Policy{
		Version: 1, Verify: &on,
		Review: []ReviewRule{{Repo: "acme/*", Reviewers: []string{"a"}}},
		Layers: []Layer{
			{Scope: ScopeOrg, Org: "acme", Verify: &off,
				Review: []ReviewRule{{Repo: "acme/*", Reviewers: []string{"b"}}, {Repo: "acme/app", GitHub: "skip"}}},
		},
	}
	got := d.Resolve(Target{Repo: "acme/app"})
	if got.verify() {
		t.Fatal("org layer should turn verify off")
	}
	if len(got.Review) != 2 || got.reviewFor("acme/lib").Reviewers[0] != "b" || got.reviewFor("acme/app").GitHub != "skip" {
		t.Fatalf("review rules not merged by repo key: %+v", got.Review)
	}
	d.Locked = true
	if !d.Resolve(Target{Repo: "acme/app"}).verify() {
		t.Fatal("locked workspace verify must win")
	}
}

func TestResolveFlattens(t *testing.T) {
	if got := ladderDoc().Resolve(Target{Repo: "acme/app"}); len(got.Layers) != 0 {
		t.Fatalf("resolved policy still has layers: %+v", got.Layers)
	}
}

func TestValidateLayers(t *testing.T) {
	bad := map[string]Layer{
		"unknown scope": {Scope: "team"},
		"org no name":   {Scope: ScopeOrg},
		"repo no slash": {Scope: ScopeRepo, Repo: "app"},
		"path no repo":  {Scope: ScopePath, Path: "a"},
		"path no path":  {Scope: ScopePath, Repo: "a/b"},
		"path escapes":  {Scope: ScopePath, Repo: "a/b", Path: "../x"},
		"bad plugin":    {Scope: ScopeOrg, Org: "a", Plugins: []Plugin{{Name: "p", Action: "nope"}}},
	}
	for name, l := range bad {
		t.Run(name, func(t *testing.T) {
			p := &Policy{Version: 1, Layers: []Layer{l}}
			if err := p.validate(); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestLoadPolicyResolvesWorkspaceLevel(t *testing.T) {
	raw := `{"version":1,"plugins":[{"name":"x","match":{"tool":"gh"},"action":"deny","message":"ws"}],
	 "layers":[{"scope":"org","org":"acme","plugins":[{"name":"x","match":{"tool":"gh"},"action":"pass"}]}]}`
	f := filepath.Join(t.TempDir(), "p.json")
	if err := os.WriteFile(f, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(raw))
	p, err := LoadPolicy(f, hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	if x := plug(p, "x"); x == nil || x.Action != ActionDeny || len(p.Layers) != 0 {
		t.Fatalf("loaded policy should be the workspace resolve: %+v", p)
	}
	if _, err := LoadPolicy(f, strings.Repeat("0", 64)); err == nil {
		t.Fatal("digest mismatch must fail")
	}
}
