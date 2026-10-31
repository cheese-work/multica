package daemon

import (
	"path/filepath"
	"testing"
)

func TestStableCwdFor(t *testing.T) {
	cfg := Config{StableCwdRoot: "/s", StableCwdAgents: []string{"x99-claude-sonnet"}}
	cases := []struct {
		name                         string
		cfg                          Config
		provider, agentName, agentID string
		want                         string
	}{
		{"listed claude agent", cfg, "claude", "x99-claude-sonnet", "id1", filepath.Join("/s", "id1")},
		{"other agent", cfg, "claude", "tofu", "id2", ""},
		{"non-claude provider", cfg, "codex", "x99-claude-sonnet", "id1", ""},
		{"no root = off", Config{StableCwdAgents: []string{"x99-claude-sonnet"}}, "claude", "x99-claude-sonnet", "id1", ""},
		{"empty agent id", cfg, "claude", "x99-claude-sonnet", "", ""},
	}
	for _, c := range cases {
		if got := c.cfg.stableCwdFor(c.provider, c.agentName, c.agentID); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestActiveRepoCheckoutTaskHostPath(t *testing.T) {
	task := activeRepoCheckoutTask{WorkDir: "/w/task-1/workdir", StableDir: "/s/id1"}
	for in, want := range map[string]string{
		"/s/id1":            "/w/task-1/workdir",
		"/s/id1/sub":        "/w/task-1/workdir/sub",
		"/s/id10":           "/s/id10", // sibling prefix is not inside
		"/w/task-1/workdir": "/w/task-1/workdir",
	} {
		if got := task.hostPath(in); got != want {
			t.Errorf("hostPath(%q) = %q, want %q", in, got, want)
		}
	}
	if got := (activeRepoCheckoutTask{WorkDir: "/w"}).hostPath("/x"); got != "/x" {
		t.Errorf("no StableDir must be identity, got %q", got)
	}
}
