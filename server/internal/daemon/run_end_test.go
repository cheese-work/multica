package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/broker"
	"github.com/multica-ai/multica/server/internal/daemon/gatesuite"
	"github.com/multica-ai/multica/server/internal/daemon/sandbox"
)

// E1 and E2 (CHE-10369, plan RSC-1 R2): a run that ends by cancellation or by
// deadline still reaches runBrk.Close, so the provider rejects the token the
// run held. The run goes through runTask with a fake backend that holds its
// process open until it is stopped.

// specLauncher is a runLauncher that records the spec of each command it wraps.
// With inner nil it leaves the command unchanged; otherwise it hands the command
// to inner, so a real sandbox and run cgroup are used.
type specLauncher struct {
	inner runLauncher
	mu    sync.Mutex
	specs []sandbox.Spec
}

func (l *specLauncher) Wrap(cmd *exec.Cmd, spec sandbox.Spec) (*sandbox.Run, error) {
	l.mu.Lock()
	l.specs = append(l.specs, spec)
	l.mu.Unlock()
	if l.inner != nil {
		return l.inner.Wrap(cmd, spec)
	}
	return &sandbox.Run{Cmd: cmd}, nil
}

func (l *specLauncher) SelfCheck(ctx context.Context) error {
	if l.inner != nil {
		return l.inner.SelfCheck(ctx)
	}
	return nil
}

func (l *specLauncher) firstSpec() (sandbox.Spec, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.specs) == 0 {
		return sandbox.Spec{}, false
	}
	return l.specs[0], true
}

// cgroupSandbox is a real bwrap launcher whose run cgroups are created under a
// fresh directory of the test's own cgroup, so a test can see them. It returns
// nil when the host cannot run the sandbox.
func cgroupSandbox(t *testing.T) (*sandbox.Launcher, string) {
	t.Helper()
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return nil, ""
	}
	rel, ok := "", false
	for _, line := range strings.Split(string(raw), "\n") {
		if rel, ok = strings.CutPrefix(line, "0::"); ok {
			break
		}
	}
	if !ok {
		return nil, ""
	}
	root, err := os.MkdirTemp(filepath.Join("/sys/fs/cgroup", rel), "e3-")
	if err != nil {
		return nil, ""
	}
	t.Cleanup(func() { _ = os.Remove(root) })
	l := &sandbox.Launcher{Mode: sandbox.ModeOn, CgroupRoot: root}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := l.SelfCheck(ctx); err != nil {
		return nil, ""
	}
	return l, root
}

// runCgroups lists the run cgroups under root.
func runCgroups(root string) []string {
	ents, _ := os.ReadDir(root)
	var out []string
	for _, e := range ents {
		if e.IsDir() && strings.HasPrefix(e.Name(), "multica-run-") {
			out = append(out, filepath.Join(root, e.Name()))
		}
	}
	return out
}

// fakeBlockedClaude answers the handshake, marks itself ready and then waits.
const fakeBlockedClaude = `#!/bin/sh
case "$*" in *--version*) echo 9.9.9; exit 0;; esac
IFS= read -r _
printf '%s\n' '{"type":"system","session_id":"s1"}'
echo up > "$READY_FILE"
exec tail -f /dev/null
`

// blockedRun is one runTask in flight with a read token held by its run.
type blockedRun struct {
	gh     *gatesuite.GitHub
	token  string // never printed
	done   chan struct{}
	result TaskResult
	err    error

	launcher   *specLauncher
	mu         sync.Mutex
	revokes    []bool // per revoke GitHub received: was the run launch already closed
	apiDir     string // the run launch's API socket dir, removed by runLaunch.Close
	cgroupRoot string // set with a real sandbox: the run's cgroup must be gone too
}

// watchRevokes puts a proxy between the broker and GitHub that notes, at each
// token revoke, whether the run launch has already been closed.
func (b *blockedRun) watchRevokes(t *testing.T, cfg *BrokerConfig) {
	t.Helper()
	target, err := url.Parse(cfg.APIBase)
	if err != nil {
		t.Fatal(err)
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/installation/token" {
			b.mu.Lock()
			_, statErr := os.Stat(b.apiDir)
			closed := b.apiDir != "" && os.IsNotExist(statErr)
			if b.cgroupRoot != "" && len(runCgroups(b.cgroupRoot)) > 0 {
				closed = false
			}
			b.revokes = append(b.revokes, closed)
			b.mu.Unlock()
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	cfg.APIBase = srv.URL
}

// probe answers the status GitHub gives the held token on a read.
func (b *blockedRun) probe(t *testing.T) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, b.gh.URL+"/repos/o/r/pulls/1", nil)
	req.Header.Set("Authorization", "Bearer "+b.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (b *blockedRun) waitDone(t *testing.T) {
	t.Helper()
	select {
	case <-b.done:
	case <-time.After(30 * time.Second):
		t.Fatal("runTask did not return")
	}
}

// startBlockedRun starts runTask under ctx, waits for the fake backend, pins the
// run's broker to a checkout (the daemon does this at the first repo checkout)
// and takes a read token through the run's own socket.
func startBlockedRun(t *testing.T, ctx context.Context, agentTimeout time.Duration) *blockedRun {
	t.Helper()
	return startBlockedRunWatching(t, ctx, agentTimeout, false, nil, "")
}

// startBlockedRunWatching is startBlockedRun with an optional revoke watch and
// an optional real sandbox (with the cgroup root its run cgroups appear under).
func startBlockedRunWatching(t *testing.T, ctx context.Context, agentTimeout time.Duration, watch bool, real *sandbox.Launcher, cgroupRoot string) *blockedRun {
	t.Helper()
	launcher := &specLauncher{}
	if real != nil {
		launcher.inner = real
	}
	d, task, _ := launchDaemon(t, sandbox.ModeOn, launcher)
	gh := gatesuite.NewGitHub(t, gatesuite.GitHubOptions{Repos: map[string]int64{"o/r": 42}})
	d.cfg.Broker = brokerTestConfig(t, gh)
	d.cfg.AgentTimeout = agentTimeout
	ready := filepath.Join(t.TempDir(), "ready")
	task.Agent.CustomEnv["READY_FILE"] = ready
	writeTestExecutable(t, d.cfg.Agents["claude"].Path, []byte(fakeBlockedClaude))

	b := &blockedRun{gh: gh, done: make(chan struct{}), launcher: launcher, cgroupRoot: cgroupRoot}
	if watch {
		b.watchRevokes(t, d.cfg.Broker)
	}
	go func() {
		defer close(b.done)
		b.result, b.err = d.runTask(ctx, task, "claude", 0, quietLog())
	}()
	// The fake backend signals through a file; inside a real sandbox it cannot
	// reach the host, so the run's cgroup holding a process is the signal.
	isReady := func() bool {
		if real != nil {
			for _, cg := range runCgroups(cgroupRoot) {
				if procs, _ := os.ReadFile(filepath.Join(cg, "cgroup.procs")); len(strings.TrimSpace(string(procs))) > 0 {
					return true
				}
			}
			return false
		}
		_, err := os.Stat(ready)
		return err == nil
	}
	for range 400 {
		if isReady() {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !isReady() {
		t.Fatal("fake backend never became ready")
	}

	if spec, ok := launcher.firstSpec(); ok {
		b.mu.Lock()
		b.apiDir = filepath.Dir(spec.DaemonSocket)
		b.mu.Unlock()
	}

	d.repoCheckoutTasksMu.RLock()
	var rb *runBroker
	for _, a := range d.repoCheckoutTasks {
		rb = a.Broker
	}
	d.repoCheckoutTasksMu.RUnlock()
	if rb == nil {
		t.Fatal("runTask registered no run broker")
	}
	if err := rb.start(brokerCheckout{TaskID: task.ID, RepoURL: "https://github.com/o/r.git", Branch: "agent/x/t1",
		Base: strings.Repeat("a", 40), Objects: t.TempDir(), Bases: []string{"main"},
		Policy: func(context.Context) ([]string, error) { return []string{"AGENTS.md"}, nil }}); err != nil {
		t.Fatalf("start broker: %v", err)
	}
	tok, err := broker.GetReadToken(context.Background(), rb.Socket)
	if err != nil {
		t.Fatalf("read token: %v", err)
	}
	b.token = tok.Token
	if got := b.probe(t); got != http.StatusOK {
		t.Fatalf("the run's token answers %d during the run, want 200", got)
	}
	return b
}

func TestTokenRejectedAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := startBlockedRun(t, ctx, time.Minute)
	cancel()
	b.waitDone(t)
	if got := b.probe(t); got != http.StatusUnauthorized {
		t.Fatalf("after cancel the token answers %d, want 401", got)
	}
	if u := b.gh.Unrevoked(); len(u) != 0 {
		t.Fatalf("mints still live after cancel: %v", u)
	}
}

func TestTokenRejectedAfterTimeout(t *testing.T) {
	b := startBlockedRun(t, context.Background(), 5*time.Second)
	b.waitDone(t)
	if b.result.FailureReason != "timeout" {
		t.Fatalf("runTask failure reason %q, want timeout: the run must end by its deadline, not by cancel", b.result.FailureReason)
	}
	if got := b.probe(t); got != http.StatusUnauthorized {
		t.Fatalf("after the run timed out the token answers %d, want 401 (runTask: %+v, %v)", got, b.result, b.err)
	}
	if u := b.gh.Unrevoked(); len(u) != 0 {
		t.Fatalf("mints still live after timeout: %v", u)
	}
}

// E3 (CHE-10369, plan RSC-1 R2): runTask closes the run launch (which kills the
// run's processes and removes its cgroup) before the broker revokes the token,
// so no process of the run outlives its token. At each revoke that reaches
// GitHub, the launch must already be closed: its API socket dir is gone and,
// when the host can run the sandbox, the run's real cgroup is gone too.
func TestLaunchClosesBeforeBrokerRevoke(t *testing.T) {
	real, cgroupRoot := cgroupSandbox(t)
	if real == nil {
		t.Log("host cannot run the sandbox: E3 checks the launch's API socket dir only, not the cgroup")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := startBlockedRunWatching(t, ctx, time.Minute, true, real, cgroupRoot)
	b.mu.Lock()
	dir := b.apiDir
	b.mu.Unlock()
	if dir == "" {
		t.Fatal("the run's launch recorded no daemon socket")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the run launch's API dir is gone during the run: %v", err)
	}
	if real != nil && len(runCgroups(cgroupRoot)) != 1 {
		t.Fatalf("run cgroups during the run: %v, want exactly one", runCgroups(cgroupRoot))
	}
	cancel()
	b.waitDone(t)
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.revokes) == 0 {
		t.Fatal("GitHub saw no token revoke")
	}
	for i, closed := range b.revokes {
		if !closed {
			t.Fatalf("revoke %d reached GitHub while the run launch was still open (cgroups left: %v): a run process could outlive its token", i+1, runCgroups(cgroupRoot))
		}
	}
}
