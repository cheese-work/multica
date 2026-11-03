package broker

import (
	"context"
	"errors"
	"io"
	"regexp"
	"sync"
	"testing"
	"time"
)

// Broker side of the revocation fence (CHE-2216 plan 3.8, slice B4b; T28 (c) to (e)).

type fakeOp struct{ scope, hash, result string }

// fakeAuthority is the server: it records what the broker authorizes and reports.
type fakeAuthority struct {
	mu          sync.Mutex
	ops         []*fakeOp
	authorizeIn error
	completeErr error
	completes   int
}

func (a *fakeAuthority) Authorize(_ context.Context, scope, hash string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.authorizeIn != nil {
		return "", a.authorizeIn
	}
	a.ops = append(a.ops, &fakeOp{scope: scope, hash: hash})
	return string(rune('a' + len(a.ops) - 1)), nil
}

func (a *fakeAuthority) Complete(_ context.Context, opID, result string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.completes++
	if a.completeErr != nil {
		return a.completeErr
	}
	a.ops[int(opID[0]-'a')].result = result
	return nil
}

func (a *fakeAuthority) results() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, o := range a.ops {
		out = append(out, o.result)
	}
	return out
}

func (a *fakeAuthority) authorized() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.ops)
}

func elevatedRig(t *testing.T) (*rig, *Broker, *fakeAuthority) {
	t.Helper()
	r := newRig(t)
	a := &fakeAuthority{}
	r.pin.Elevation = &Elevation{Authority: a, Scope: "repo_write:42", ReportFor: 100 * time.Millisecond}
	r.commits(t, 1)
	b := r.start(t)
	return r, b, a
}

// gate blocks a hook until released and says when it was reached.
type gate struct{ reached, release chan struct{} }

func newGate() *gate  { return &gate{make(chan struct{}), make(chan struct{})} }
func (g *gate) hook() { close(g.reached); <-g.release }

// wait fails the test, rather than hang it, when the hook is never reached.
func (g *gate) wait(t *testing.T) {
	t.Helper()
	select {
	case <-g.reached:
	case <-time.After(60 * time.Second):
		t.Fatal("the remote-write hook was never reached")
	}
}

func pushAsync(r *rig) chan error {
	done := make(chan error, 1)
	go func() { done <- Push(context.Background(), r.socket, r.work, io.Discard) }()
	return done
}

func TestElevatedPushBindsInputAndReports(t *testing.T) {
	t.Parallel()
	r, _, a := elevatedRig(t)
	if err := Push(context.Background(), r.socket, r.work, io.Discard); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got, want := r.remoteRef(t, r.pin.Ref), git(t, r.work, "rev-parse", "HEAD"); got != want {
		t.Fatalf("remote %s = %q, want %q", r.pin.Ref, got, want)
	}
	if res := a.results(); len(res) != 1 || res[0] != "completed" {
		t.Fatalf("reported %v, want one completed", res)
	}
	first := a.ops[0]
	if first.scope != "repo_write:42" || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(first.hash) {
		t.Fatalf("authorized %+v, want the pinned scope and a SHA-256 input hash", first)
	}
	r.commits(t, 1) // different bytes must bind to a different hash
	if err := Push(context.Background(), r.socket, r.work, io.Discard); err != nil {
		t.Fatalf("second Push: %v", err)
	}
	if a.ops[1].hash == first.hash {
		t.Fatal("two different bundles authorized under one input hash")
	}
}

// T28 (c): authorized at epoch N, paused before the remote write, then fenced.
func TestFenceAbortsPushBeforeRemoteWrite(t *testing.T) {
	t.Parallel()
	r, b, a := elevatedRig(t)
	g := newGate()
	b.beforeWrite = g.hook
	done := pushAsync(r)
	g.wait(t)
	b.Fence()
	close(g.release)
	if err := <-done; err == nil {
		t.Fatal("push succeeded after the fence started")
	}
	if got := r.remoteRef(t, r.pin.Ref); got != "" {
		t.Fatalf("remote ref %q written after the fence", got)
	}
	if res := a.results(); len(res) != 1 || res[0] != "aborted" {
		t.Fatalf("reported %v, want one aborted", res)
	}
	if err := b.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
}

// T28 (d): the same push paused after its remote write. The fence waits for it.
func TestFenceDrainWaitsForStartedPush(t *testing.T) {
	t.Parallel()
	r, b, a := elevatedRig(t)
	g := newGate()
	b.afterWrite = g.hook
	done := pushAsync(r)
	g.wait(t)
	b.Fence()
	drained := make(chan error, 1)
	go func() { drained <- b.Drain(context.Background()) }()
	select {
	case err := <-drained:
		t.Fatalf("Drain returned (%v) while a started push had not reported", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(g.release)
	if err := <-done; err != nil {
		t.Fatalf("started push: %v", err)
	}
	if err := <-drained; err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if res := a.results(); len(res) != 1 || res[0] != "completed" {
		t.Fatalf("reported %v, want one completed", res)
	}
	if got, want := r.remoteRef(t, r.pin.Ref), git(t, r.work, "rev-parse", "HEAD"); got != want {
		t.Fatalf("remote %q, want the started push to land (%q)", got, want)
	}
}

// T28 (e): the server never hears the result, so the fence must not complete.
func TestDrainFailsWhenResultIsNotAcknowledged(t *testing.T) {
	t.Parallel()
	r, b, a := elevatedRig(t)
	a.completeErr = errors.New("server unreachable")
	if err := Push(context.Background(), r.socket, r.work, io.Discard); err != nil {
		t.Fatalf("Push: %v", err)
	}
	b.Fence()
	if err := b.Drain(context.Background()); !errors.Is(err, ErrUnreported) {
		t.Fatalf("Drain = %v, want ErrUnreported", err)
	}
	a.mu.Lock()
	a.completeErr = nil
	a.mu.Unlock()
	if err := b.Drain(context.Background()); err != nil {
		t.Fatalf("Drain after the server came back: %v", err)
	}
	if res := a.results(); res[0] != "completed" {
		t.Fatalf("reported %v, want the late completed", res)
	}
}

func TestFencedBrokerAuthorizesNothing(t *testing.T) {
	t.Parallel()
	r, b, a := elevatedRig(t)
	b.Fence()
	if err := Push(context.Background(), r.socket, r.work, io.Discard); err == nil {
		t.Fatal("push succeeded on a fenced broker")
	}
	if n := a.authorized(); n != 0 {
		t.Fatalf("%d operations authorized after the fence", n)
	}
}

func TestAuthorizeFailureFailsClosed(t *testing.T) {
	t.Parallel()
	r, _, a := elevatedRig(t)
	a.authorizeIn = errors.New("server unreachable")
	if err := Push(context.Background(), r.socket, r.work, io.Discard); err == nil {
		t.Fatal("push succeeded without authority")
	}
	if got := r.remoteRef(t, r.pin.Ref); got != "" {
		t.Fatalf("remote ref %q written without authority", got)
	}
}

func TestFencedPROperationIsRefusedBeforeGitHubWrite(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	f := newFakeGitHub(t, r.remote)
	a := &fakeAuthority{}
	r.pin.RemoteURL, r.pin.Slug, r.pin.Bases, r.pin.Tokens = f.srv.URL+"/git/remote.git", "o/r", []string{"main"}, f.tokens()
	r.pin.Elevation = &Elevation{Authority: a, Scope: "repo_write:42", ReportFor: 100 * time.Millisecond}
	b := r.start(t)
	f.addPR(r.branch, 42)
	b.Fence()
	if code, body := r.pr(t, `{"op":"comment","number":1,"body":"x"}`); code != 403 {
		t.Fatalf("status %d (%s), want 403", code, body)
	}
	if w := f.writeCalls(); len(w) != 0 {
		t.Fatalf("GitHub was written after the fence: %v", w)
	}
	if n := a.authorized(); n != 0 {
		t.Fatalf("%d operations authorized after the fence", n)
	}
}
