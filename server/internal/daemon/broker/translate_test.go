package broker

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// synRig is a rig whose run works in a synthetic-base checkout of r.base.
type synRig struct {
	*rig
	syn  string
	root string
}

func newSynRig(t testing.TB) *synRig {
	t.Helper()
	r := newRig(t)
	syn := filepath.Join(t.TempDir(), "syn")
	sb, err := MaterializeSyntheticBase(context.Background(), r.cache, r.base, syn, r.branch)
	if err != nil {
		t.Fatal(err)
	}
	r.pin.Base = sb.Root
	r.pin.Synthetic = &Synthetic{Root: sb.Root, Parent: sb.Source}
	return &synRig{rig: r, syn: syn, root: sb.Root}
}

func (s *synRig) commit(t testing.TB, file, body, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.syn, file), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, s.syn, "add", ".")
	git(t, s.syn, "commit", "-q", "-m", msg)
}

func TestElevatedPushTranslatesSyntheticRoot(t *testing.T) {
	t.Parallel()
	s := newSynRig(t)
	s.commit(t, "x.txt", "1\n", "first\n\nbody line")
	s.commit(t, "y.txt", "2\n", "second")
	s.start(t)
	if err := Push(context.Background(), s.socket, s.syn, &bytes.Buffer{}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	tip := s.remoteRef(t, s.pin.Ref)
	if tip == "" || tip == git(t, s.syn, "rev-parse", "HEAD") {
		t.Fatalf("remote tip %q: want a translated commit, not the run's own", tip)
	}
	if got := git(t, s.remote, "rev-list", "--parents", "-n1", tip+"~1"); !strings.HasSuffix(got, " "+s.base) {
		t.Fatalf("first run commit parents = %q, want the real base %s", got, s.base)
	}
	for _, rev := range []string{"HEAD", "HEAD~1"} {
		for _, f := range []string{"%T", "%an", "%ae", "%at", "%B"} {
			want := git(t, s.syn, "log", "-1", "--format="+f, rev)
			if got := git(t, s.remote, "log", "-1", "--format="+f, tip+strings.TrimPrefix(rev, "HEAD")); got != want {
				t.Fatalf("%s of %s: remote %q, run %q", f, rev, got, want)
			}
		}
	}
}

func TestElevatedPushRefusals(t *testing.T) {
	t.Parallel()
	s := newSynRig(t)
	s.start(t)
	cases := []struct {
		name  string
		setup func()
		code  int
	}{
		{"protected path", func() { s.commit(t, ".github/ci.yml", "evil\n", "ci") }, 403},
		{"history from another root", func() {
			git(t, s.syn, "checkout", "-q", "--orphan", "tmp")
			s.commit(t, "z.txt", "3\n", "orphan")
			git(t, s.syn, "branch", "-f", s.branch, "HEAD")
			git(t, s.syn, "checkout", "-q", s.branch)
		}, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.setup()
			err := Push(context.Background(), s.socket, s.syn, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), "("+strconv.Itoa(c.code)+")") {
				t.Fatalf("Push error = %v, want status %d", err, c.code)
			}
			if got := s.remoteRef(t, s.pin.Ref); got != "" {
				t.Fatalf("remote gained %s at %s", s.pin.Ref, got)
			}
		})
	}
}

// A compressible oversized commit message must be refused before translate
// reads the decompressed object into daemon memory.
func TestElevatedPushRefusesOversizedCommit(t *testing.T) {
	t.Parallel()
	s := newSynRig(t)
	s.start(t)
	if err := os.WriteFile(filepath.Join(s.syn, "big.txt"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	msg := filepath.Join(t.TempDir(), "msg")
	if err := os.WriteFile(msg, bytes.Repeat([]byte("a"), maxCommitBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, s.syn, "add", ".")
	git(t, s.syn, "commit", "-q", "-F", msg)
	err := Push(context.Background(), s.socket, s.syn, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "(413)") {
		t.Fatalf("Push error = %v, want status 413", err)
	}
	if got := s.remoteRef(t, s.pin.Ref); got != "" {
		t.Fatalf("remote gained %s at %s", s.pin.Ref, got)
	}
}
