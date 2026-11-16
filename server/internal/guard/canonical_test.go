package guard

import (
	"reflect"
	"testing"
)

func TestCanonicalCommand(t *testing.T) {
	cases := []struct {
		name    string
		argv    []string
		tool    string
		words   []string
		wrapper bool
	}{
		{"plain", []string{"multica", "daemon", "restart"}, "multica", []string{"daemon", "restart"}, false},
		{"env var", []string{"$MULTICA_CLI_PATH", "daemon", "restart"}, "multica", []string{"daemon", "restart"}, false},
		{"braced env var", []string{"${MULTICA_CLI_PATH}", "daemon", "stop"}, "multica", []string{"daemon", "stop"}, false},
		{"quotes kept in word", []string{`"$MULTICA_CLI_PATH"`, "daemon", "restart"}, "multica", []string{"daemon", "restart"}, false},
		{"single quotes", []string{`'multica'`, "daemon", "restart"}, "multica", []string{"daemon", "restart"}, false},
		{"resolved path and global flag", []string{"/home/u/.local/bin/multica", "--profile", "dev", "daemon", "restart"}, "multica", []string{"daemon", "restart"}, false},
		{"flag=value globals", []string{"multica", "--server-url=http://x", "--debug", "--workspace-id", "w", "daemon", "restart", "--maintenance-token", "x"}, "multica", []string{"daemon", "restart"}, false},
		{"windows path", []string{`C:\bin\Multica.EXE`, "daemon", "stop"}, "multica", []string{"daemon", "stop"}, false},
		{"other tool", []string{"/usr/bin/GH", "pr", "ready", "--draft"}, "gh", []string{"pr", "ready"}, false},
		{"wrapper", []string{"env", "X=1", "multica", "daemon", "restart"}, "env", []string{"X=1", "multica", "daemon", "restart"}, true},
		{"wrapper path", []string{"/bin/bash", "-c", "x"}, "bash", []string{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tool, words, wrapper := CanonicalCommand(c.argv)
			if tool != c.tool || !reflect.DeepEqual(append([]string{}, words...), c.words) || wrapper != c.wrapper {
				t.Fatalf("CanonicalCommand(%q) = %q %q %v, want %q %q %v", c.argv, tool, words, wrapper, c.tool, c.words, c.wrapper)
			}
		})
	}
}

func TestMatchCovers(t *testing.T) {
	m := Match{Tool: "multica", Subcommand: []string{"daemon", "restart"}}
	for _, c := range []struct {
		tool  string
		words []string
		want  bool
	}{
		{"multica", []string{"daemon", "restart"}, true},
		{"multica", []string{"daemon", "restart", "now"}, true},
		{"multica", []string{"daemon", "status"}, false},
		{"multica", []string{"daemon", "restarts"}, false},
		{"multica", []string{"daemon"}, false},
		{"gh", []string{"daemon", "restart"}, false},
	} {
		if got := m.Covers(c.tool, c.words); got != c.want {
			t.Errorf("Covers(%q, %q) = %v, want %v", c.tool, c.words, got, c.want)
		}
	}
}
