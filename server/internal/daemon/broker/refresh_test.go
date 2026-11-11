package broker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitErr(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func msgSum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// advanceMain lands a new upstream commit on the remote default branch and
// returns its SHA.
func (s *synRig) advanceMain(t testing.TB, file, body, msg string) string {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "up")
	git(t, filepath.Dir(clone), "clone", "-q", s.remote, clone)
	if err := os.WriteFile(filepath.Join(clone, file), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, clone, "add", ".")
	git(t, clone, "commit", "-q", "-m", msg)
	git(t, clone, "push", "-q", "origin", "main")
	return git(t, clone, "rev-parse", "HEAD")
}

// T29: the receipt names the commits the remote holds, not the run's own, and
// the broker answers whether a message is the one it pushed.
func TestElevatedReceiptsOnTranslatedSHAs(t *testing.T) {
	t.Parallel()
	s := newSynRig(t)
	s.commit(t, "x.txt", "1\n", "first\n\nbody line")
	s.commit(t, "y.txt", "2\n", "second")
	b := s.start(t)
	runTip, runPrev := git(t, s.syn, "rev-parse", "HEAD"), git(t, s.syn, "rev-parse", "HEAD~1")
	if err := Push(context.Background(), s.socket, s.syn, &bytes.Buffer{}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	tip := s.remoteRef(t, s.pin.Ref)
	rcs := b.Receipts()
	if len(rcs) != 1 || len(rcs[0].Commits) != 2 {
		t.Fatalf("receipts = %+v, want one with two commits", rcs)
	}
	first := git(t, s.remote, "rev-parse", tip+"~1")
	want := []Commit{
		{SHA: first, RunSHA: runPrev, MessageSHA256: msgSum("first\n\nbody line\n")},
		{SHA: tip, RunSHA: runTip, MessageSHA256: msgSum("second\n")},
	}
	for i, c := range rcs[0].Commits {
		if c != want[i] {
			t.Fatalf("commit %d = %+v, want %+v", i, c, want[i])
		}
	}
	for _, c := range []struct {
		name, sha, msg string
		ok             bool
	}{
		{"translated SHA, own message", tip, "second\n", true},
		{"run SHA, own message", runTip, "second\n", true},
		{"own SHA, edited message", tip, "second (edited)\n", false},
		{"another commit's message", tip, "first\n\nbody line\n", false},
		{"unknown SHA", s.base, "base\n", false},
	} {
		if got := b.OwnMessage(c.sha, []byte(c.msg)); got != c.ok {
			t.Errorf("%s: OwnMessage = %v, want %v", c.name, got, c.ok)
		}
	}
}

func TestBaseRefreshGivesTreeOnlyRoot(t *testing.T) {
	t.Parallel()
	s := newSynRig(t)
	s.commit(t, "x.txt", "1\n", "mine")
	newTip := s.advanceMain(t, "src/b.go", "package b\n", "UPSTREAM-HOSTILE-MESSAGE")
	s.start(t)
	root, err := RefreshBase(context.Background(), s.socket, s.syn)
	if err != nil {
		t.Fatalf("RefreshBase: %v", err)
	}
	if got := git(t, s.syn, "rev-list", "--parents", "-n1", root); got != root {
		t.Fatalf("new root has parents: %q", got)
	}
	if got, want := git(t, s.syn, "rev-parse", root+"^{tree}"), git(t, s.remote, "rev-parse", newTip+"^{tree}"); got != want {
		t.Fatalf("new root tree %s, want the tree of %s (%s)", got, newTip, want)
	}
	if got := git(t, s.syn, "log", "-1", "--format=%an%x00%B", root); got != "multica-broker\x00base "+newTip {
		t.Fatalf("new root = %q", got)
	}
	if out := git(t, s.syn, "log", "--all", "--format=%B"); strings.Contains(out, "UPSTREAM-HOSTILE-MESSAGE") {
		t.Fatalf("upstream message reached the checkout:\n%s", out)
	}
	if _, err := gitErr(s.syn, "cat-file", "-t", newTip); err == nil {
		t.Fatalf("upstream commit %s is readable in the checkout", newTip)
	}
	// The run rebases its own commit and pushes: the remote history sits on the new tip.
	git(t, s.syn, "rebase", "-q", "--onto", root, s.root)
	if err := Push(context.Background(), s.socket, s.syn, &bytes.Buffer{}); err != nil {
		t.Fatalf("Push after refresh: %v", err)
	}
	if got := git(t, s.remote, "rev-parse", s.remoteRef(t, s.pin.Ref)+"~1"); got != newTip {
		t.Fatalf("pushed commit's parent = %s, want the new default tip %s", got, newTip)
	}
}

func TestBaseRefreshRefusals(t *testing.T) {
	t.Parallel()
	s := newSynRig(t)
	b := s.start(t)
	// At the default tip a refresh is a no-op that returns the same root, so a
	// client that failed after the pin moved can simply retry.
	root, err := RefreshBase(context.Background(), s.socket, s.syn)
	if err != nil || root != s.root {
		t.Fatalf("refresh at the default tip = %q, %v; want the unchanged root %s", root, err, s.root)
	}
	// A default branch rewritten to unrelated history is refused.
	clone := filepath.Join(t.TempDir(), "rw")
	git(t, filepath.Dir(clone), "clone", "-q", s.remote, clone)
	git(t, clone, "checkout", "-q", "--orphan", "rewritten")
	git(t, clone, "commit", "-q", "-m", "rewritten", "--allow-empty")
	git(t, clone, "push", "-q", "-f", "origin", "rewritten:main")
	if _, err := RefreshBase(context.Background(), s.socket, s.syn); err == nil || !strings.Contains(err.Error(), "(409)") {
		t.Fatalf("refresh after a history rewrite: error = %v, want status 409", err)
	}
	b.Fence()
	if _, err := RefreshBase(context.Background(), s.socket, s.syn); err == nil || !strings.Contains(err.Error(), "(403)") {
		t.Fatalf("refresh while fenced: error = %v, want status 403", err)
	}

	r := newRig(t)
	r.start(t)
	if _, err := RefreshBase(context.Background(), r.socket, r.work); err == nil || !strings.Contains(err.Error(), "(400)") {
		t.Fatalf("refresh on a run with no synthetic base: error = %v, want status 400", err)
	}
}

func elevatedSynRig(t *testing.T) (*synRig, *Broker, *fakeAuthority) {
	t.Helper()
	s := newSynRig(t)
	a := &fakeAuthority{}
	s.pin.Elevation = &Elevation{Authority: a, Scope: "repo_write:42", ReportFor: 100 * time.Millisecond}
	s.advanceMain(t, "src/b.go", "package b\n", "upstream")
	b := s.start(t)
	return s, b, a
}

// A refresh runs the same live per-operation authority as a push: the server's
// refusal (a revoked grant whose fence notice has not arrived) stops the fetch,
// and a granted one is reported as completed.
func TestBaseRefreshNeedsLiveAuthority(t *testing.T) {
	t.Parallel()
	s, _, a := elevatedSynRig(t)
	a.authorizeIn = errors.New("grant revoked")
	if _, err := RefreshBase(context.Background(), s.socket, s.syn); err == nil || !strings.Contains(err.Error(), "(503)") {
		t.Fatalf("refresh after revocation: error = %v, want status 503", err)
	}
	if _, err := gitErr(s.syn, "rev-parse", "--verify", "refs/multica/base"); err == nil {
		t.Fatal("a refused refresh delivered a new base")
	}
	a.mu.Lock()
	a.authorizeIn = nil
	a.mu.Unlock()
	root, err := RefreshBase(context.Background(), s.socket, s.syn)
	if err != nil || root == s.root {
		t.Fatalf("authorized refresh = %q, %v; want a new root", root, err)
	}
	if res := a.results(); len(res) != 1 || res[0] != "completed" {
		t.Fatalf("reported %v, want one completed", res)
	}
}

// A fence that begins while the server is authorizing the refresh: the refresh
// is refused, the abort is reported, and Drain has waited for it.
func TestBaseRefreshFencedMidAuthorizationAborts(t *testing.T) {
	t.Parallel()
	s, b, a := elevatedSynRig(t)
	a.authGate = newGate()
	done := make(chan error, 1)
	go func() { _, err := RefreshBase(context.Background(), s.socket, s.syn); done <- err }()
	a.authGate.wait(t)
	b.Fence()
	drained := make(chan error, 1)
	go func() { drained <- b.Drain(context.Background()) }()
	select {
	case err := <-drained:
		t.Fatalf("Drain returned (%v) while a refresh was authorizing", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(a.authGate.release)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "(403)") {
		t.Fatalf("refresh fenced mid-authorization: error = %v, want status 403", err)
	}
	if err := <-drained; err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if res := a.results(); len(res) != 1 || res[0] != "aborted" {
		t.Fatalf("reported %v, want one aborted", res)
	}
	if _, err := gitErr(s.syn, "rev-parse", "--verify", "refs/multica/base"); err == nil {
		t.Fatal("a fenced refresh delivered a new base")
	}
}

// A refresh in flight when the fence begins finishes its read before Drain
// returns, but delivers nothing once the fence has begun.
func TestBaseRefreshFencedBeforeReplyDeliversNothing(t *testing.T) {
	t.Parallel()
	s, b, a := elevatedSynRig(t)
	g := newGate()
	b.beforeWrite = g.hook
	done := make(chan error, 1)
	go func() { _, err := RefreshBase(context.Background(), s.socket, s.syn); done <- err }()
	g.wait(t)
	b.Fence()
	close(g.release)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "(403)") {
		t.Fatalf("refresh fenced before its reply: error = %v, want status 403", err)
	}
	if err := b.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if res := a.results(); len(res) != 1 || res[0] != "aborted" {
		t.Fatalf("reported %v, want one aborted", res)
	}
	if _, err := gitErr(s.syn, "rev-parse", "--verify", "refs/multica/base"); err == nil {
		t.Fatal("a fenced refresh delivered a new base")
	}
}
