package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// J4: the credential journal root and its key are daemon-private. No bind may
// contain them, and DefaultDeny must name them.
func TestJournalRootNotInSandboxBinds(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	base := CredJournalBase()
	if want := filepath.Join(state, "multica", "cred-journal"); base != want {
		t.Fatalf("CredJournalBase = %q, want %q", base, want)
	}
	root := filepath.Join(base, "default")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(root, "journal.key")
	entry := filepath.Join(root, "0123456789abcdef")
	for _, p := range []string{key, entry} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	listed := false
	for _, d := range DefaultDeny() {
		if d == base {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("DefaultDeny %v does not list %s", DefaultDeny(), base)
	}
	link := filepath.Join(t.TempDir(), "innocent")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	ly := newLayout(t)
	cases := map[string]string{
		"state home":     state,
		"journal parent": filepath.Join(state, "multica"),
		"journal base":   base,
		"journal root":   root,
		"journal key":    key,
		"entry":          entry,
		"symlink":        link,
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			l := newLauncher()
			spec := ly.spec("true")
			spec.ReadOnly = []string{p}
			_, err := l.Args(spec)
			if err == nil || !strings.Contains(err.Error(), "never-mounted") {
				t.Fatalf("bind %s: err = %v, want a never-mounted refusal", p, err)
			}
		})
	}
}
