package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

func TestStableCwdFor(t *testing.T) {
	root := t.TempDir()
	cfg := Config{WorkspacesRoot: root, StableCwdAgents: []string{"x99-claude-sonnet"}}
	cases := []struct {
		name                         string
		cfg                          Config
		provider, agentName, agentID string
		want                         string
	}{
		{"listed claude agent", cfg, "claude", "x99-claude-sonnet", "id1", filepath.Join(root, ".stable-cwd", "id1")},
		{"other agent", cfg, "claude", "tofu", "id2", ""},
		{"non-claude provider", cfg, "codex", "x99-claude-sonnet", "id1", ""},
		{"no agents listed = off", Config{WorkspacesRoot: root}, "claude", "x99-claude-sonnet", "id1", ""},
		{"empty agent id", cfg, "claude", "x99-claude-sonnet", "", ""},
	}
	for _, c := range cases {
		if got := c.cfg.stableCwdFor(c.provider, c.agentName, c.agentID); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// An environment-stripped child that walks up from the stable cwd must still
// meet a daemon-task marker, as it does from a real task workdir (CHE-2694 review).
func TestStableCwdKeepsDaemonTaskMarkerAncestor(t *testing.T) {
	root := t.TempDir()
	if err := execenv.EnsureWorkspacesRootMarker(root); err != nil {
		t.Fatal(err)
	}
	stable := Config{WorkspacesRoot: root, StableCwdAgents: []string{"a"}}.stableCwdFor("claude", "a", "id1")
	if stable == "" {
		t.Fatal("stable cwd off for a listed agent")
	}
	for dir := filepath.Dir(stable); ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, execenv.TaskContextMarkerRelPath)); err == nil {
			return
		}
		if dir == filepath.Dir(dir) {
			t.Fatalf("no daemon-task marker above %s", stable)
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
