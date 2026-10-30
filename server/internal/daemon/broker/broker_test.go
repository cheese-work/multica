package broker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// rig is a local bare "GitHub" remote, a bare cache clone of it, and a task
// checkout that borrows the cache's objects through alternates (the A2c
// layout) with a task branch off the base.
type rig struct {
	remote, cache, work, base, branch string
	pin                               Pin
	socket                            string
	gen                               int
}

func newRig(t testing.TB) *rig {
	t.Helper()
	root := t.TempDir()
	r := &rig{
		remote: filepath.Join(root, "remote.git"),
		cache:  filepath.Join(root, "cache.git"),
		work:   filepath.Join(root, "work"),
		branch: "agent/x/task-1",
		socket: filepath.Join(root, "b.sock"),
	}
	seed := filepath.Join(root, "seed")
	for _, d := range []string{r.remote, seed} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	git(t, r.remote, "init", "--bare", "-b", "main")
	git(t, seed, "init", "-b", "main")
	git(t, seed, "commit", "--allow-empty", "-m", "base")
	git(t, seed, "push", r.remote, "main")
	r.base = git(t, seed, "rev-parse", "HEAD")
	git(t, root, "clone", "--bare", r.remote, r.cache)
	git(t, root, "clone", "--shared", "--no-checkout", r.cache, r.work)
	git(t, r.work, "checkout", "-b", r.branch, r.base)
	// The sandbox cannot reach the cache's refs; drop the origin the clone made.
	git(t, r.work, "remote", "remove", "origin")
	r.pin = Pin{
		Repo: "42", Ref: "refs/heads/" + r.branch, Base: r.base,
		RemoteURL: r.remote, Objects: filepath.Join(r.cache, "objects"),
	}
	return r
}

// commits adds n commits to the task branch with one git process. Each call
// writes distinct commits, so a second call after a reset forks history.
func (r *rig) commits(t testing.TB, n int) {
	t.Helper()
	r.gen++
	var in strings.Builder
	for i := 0; i < n; i++ {
		from := ""
		if i == 0 {
			from = "from " + git(t, r.work, "rev-parse", "HEAD") + "\n"
		}
		fmt.Fprintf(&in, "commit %s\ncommitter t <t@example.com> %d +0000\ndata <<M\ng%d c%d\nM\n%sM 644 inline f%d.txt\ndata <<F\n%d\nF\n\n", r.pin.Ref, 1700000000+i, r.gen, i, from, i, i)
	}
	cmd := exec.Command("git", "-C", r.work, "fast-import", "--quiet", "--force")
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	cmd.Stdin = strings.NewReader(in.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fast-import: %v\n%s", err, out)
	}
	git(t, r.work, "reset", "--hard", "-q", r.pin.Ref)
}

func (r *rig) start(t testing.TB) *Broker {
	t.Helper()
	b, err := New(r.pin, r.socket, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

func (r *rig) remoteRef(t testing.TB, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", r.remote, "rev-parse", "--verify", "--quiet", ref)
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

func (r *rig) post(t testing.TB, query string, body io.Reader) (int, string) {
	t.Helper()
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", r.socket)
	}}}
	resp, err := c.Post("http://broker/push?"+query, "application/octet-stream", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestPushTaskBranch(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.commits(t, 3)
	r.start(t)
	var out bytes.Buffer
	if err := Push(context.Background(), r.socket, r.work, &out); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got, want := r.remoteRef(t, r.pin.Ref), git(t, r.work, "rev-parse", "HEAD"); got != want {
		t.Fatalf("remote %s = %q, want %q", r.pin.Ref, got, want)
	}
	if got := r.remoteRef(t, "refs/heads/main"); got != r.base {
		t.Fatalf("main moved to %q", got)
	}
}

func TestPushRefusals(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.commits(t, 1)
	r.start(t)
	bundle := func(ref string) []byte {
		if ref != r.pin.Ref {
			git(t, r.work, "branch", "-f", strings.TrimPrefix(ref, "refs/heads/"), "HEAD")
		}
		out, err := exec.Command("git", "-C", r.work, "bundle", "create", "-", ref, "^"+r.base).Output()
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	own, other := bundle(r.pin.Ref), bundle("refs/heads/main2")
	git(t, r.work, "tag", "v1")
	tag, err := exec.Command("git", "-C", r.work, "bundle", "create", "-", "refs/tags/v1", "^"+r.base).Output()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, query string
		body        []byte
		wantCode    int
		touched     string // remote ref that must stay absent
	}{
		{"other branch asked", "ref=refs/heads/main2", other, 403, "refs/heads/main2"},
		{"default branch asked", "ref=refs/heads/main", own, 403, ""},
		{"tag asked", "ref=refs/tags/v1", tag, 403, "refs/tags/v1"},
		{"bundle holds another ref", "ref=" + r.pin.Ref, other, 400, "refs/heads/main2"},
		{"bundle holds a tag", "ref=" + r.pin.Ref, tag, 400, "refs/tags/v1"},
		{"other repository", "ref=" + r.pin.Ref + "&repo=43", own, 403, r.pin.Ref},
		{"empty bundle", "ref=" + r.pin.Ref, []byte("junk"), 400, r.pin.Ref},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, body := r.post(t, c.query, bytes.NewReader(c.body))
			if code != c.wantCode {
				t.Fatalf("status %d (%s), want %d", code, body, c.wantCode)
			}
			if c.touched != "" && r.remoteRef(t, c.touched) != "" {
				t.Fatalf("remote gained %s", c.touched)
			}
			if got := r.remoteRef(t, "refs/heads/main"); got != r.base {
				t.Fatalf("main moved to %q", got)
			}
		})
	}
}

func TestPushRejectsNonFastForward(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.commits(t, 1)
	r.start(t)
	if err := Push(context.Background(), r.socket, r.work, io.Discard); err != nil {
		t.Fatal(err)
	}
	first := r.remoteRef(t, r.pin.Ref)
	git(t, r.work, "reset", "--hard", r.base)
	r.commits(t, 2)
	if err := Push(context.Background(), r.socket, r.work, io.Discard); err == nil {
		t.Fatal("a rewrite of the remote branch was pushed without force")
	}
	if got := r.remoteRef(t, r.pin.Ref); got != first {
		t.Fatalf("remote branch moved to %q", got)
	}
}

func TestPushFromWrongBranchOrDetachedHead(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.commits(t, 1)
	r.start(t)
	git(t, r.work, "checkout", "-b", "other")
	if err := Push(context.Background(), r.socket, r.work, io.Discard); err == nil {
		t.Fatal("pushed a branch the broker is not pinned to")
	}
	git(t, r.work, "checkout", "--detach")
	if err := Push(context.Background(), r.socket, r.work, io.Discard); err == nil {
		t.Fatal("pushed from a detached HEAD")
	}
	if got := r.remoteRef(t, "refs/heads/other"); got != "" {
		t.Fatalf("remote gained other: %q", got)
	}
}

func TestSocketIsOwnerOnly(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.start(t)
	fi, err := os.Stat(r.socket)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v, want 0600", fi.Mode().Perm())
	}
}

func TestNewRejectsBadPin(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	for name, mut := range map[string]func(*Pin){
		"tag ref":       func(p *Pin) { p.Ref = "refs/tags/v1" },
		"default ref":   func(p *Pin) { p.Ref = "main" },
		"bad ref":       func(p *Pin) { p.Ref = "refs/heads/a..b" },
		"no repo":       func(p *Pin) { p.Repo = "" },
		"no remote":     func(p *Pin) { p.RemoteURL = "" },
		"relative objs": func(p *Pin) { p.Objects = "objects" },
	} {
		t.Run(name, func(t *testing.T) {
			p := r.pin
			mut(&p)
			if b, err := New(p, r.socket, nil); err == nil {
				b.Close()
				t.Fatal("New accepted the pin")
			}
		})
	}
}

func TestShim(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	fake := filepath.Join(dir, "multica")
	script := "#!/bin/sh\necho \"multica $*\" >> " + log + "\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	realGit := filepath.Join(dir, "realgit")
	if err := os.WriteFile(realGit, []byte("#!/bin/sh\necho \"git $*\" >> "+log+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	shim, err := WriteShim(t.TempDir(), realGit, fake)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"push", "origin", "HEAD"}, {"status", "-s"}, {"-C", "x", "log"}} {
		if out, err := exec.Command(shim, args...).CombinedOutput(); err != nil {
			t.Fatalf("shim %v: %v %s", args, err, out)
		}
	}
	b, _ := os.ReadFile(log)
	want := "multica git push origin HEAD\ngit status -s\ngit -C x log\n"
	if string(b) != want {
		t.Fatalf("calls:\n%s\nwant:\n%s", b, want)
	}
}

// M4: added push latency, p95 at most 5 s for a 100-commit branch.
func TestPushLatency100Commits(t *testing.T) {
	if testing.Short() {
		t.Skip("measurement")
	}
	const runs = 10
	var d []time.Duration
	for i := 0; i < runs; i++ {
		r := newRig(t)
		r.commits(t, 100)
		r.start(t)
		t0 := time.Now()
		if err := Push(context.Background(), r.socket, r.work, io.Discard); err != nil {
			t.Fatal(err)
		}
		d = append(d, time.Since(t0))
	}
	sortDurations(d)
	// Baseline: this host's cost of starting git at all (the push path forks it several times).
	t0 := time.Now()
	_ = exec.Command("git", "--version").Run()
	t.Logf("host load baseline: one `git --version` takes %v; /proc/loadavg %s", time.Since(t0), loadavg())
	p95 := d[(len(d)*95+99)/100-1]
	t.Logf("M4 push of 100 commits: n=%d min=%v p50=%v p95=%v max=%v", len(d), d[0], d[len(d)/2], p95, d[len(d)-1])
	if p95 > 5*time.Second {
		t.Fatalf("p95 %v exceeds 5s", p95)
	}
}

// M3 part: broker memory with 20 concurrent runs, 100 MB at most.
func TestBrokerRSS20Runs(t *testing.T) {
	if testing.Short() || runtime.GOOS != "linux" {
		t.Skip("measurement")
	}
	const n = 20
	rigs := make([]*rig, n)
	for i := range rigs {
		rigs[i] = newRig(t)
		rigs[i].commits(t, 100)
	}
	before := rssBytes(t)
	for _, r := range rigs {
		r.start(t)
	}
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for _, r := range rigs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- Push(context.Background(), r.socket, r.work, io.Discard)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	after := rssBytes(t)
	t.Logf("M3 part: %d brokers, 20 concurrent 100-commit pushes: RSS before=%d after=%d added=%d bytes", n, before, after, after-before)
	if added := after - before; added > 100<<20 {
		t.Fatalf("added RSS %d bytes exceeds 100 MB", added)
	}
}

func rssBytes(t testing.TB) int64 {
	t.Helper()
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "VmRSS:") {
			var kb int64
			fmt.Sscanf(strings.TrimPrefix(l, "VmRSS:"), "%d", &kb)
			return kb << 10
		}
	}
	t.Fatal("no VmRSS")
	return 0
}

func sortDurations(d []time.Duration) {
	for i := 1; i < len(d); i++ {
		for j := i; j > 0 && d[j] < d[j-1]; j-- {
			d[j], d[j-1] = d[j-1], d[j]
		}
	}
}

func loadavg() string {
	b, _ := os.ReadFile("/proc/loadavg")
	return strings.TrimSpace(string(b))
}
