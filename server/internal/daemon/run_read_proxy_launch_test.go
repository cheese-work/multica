package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/gatesuite"
	"github.com/multica-ai/multica/server/internal/daemon/readproxy"
	"github.com/multica-ai/multica/server/internal/daemon/sandbox"
	"github.com/multica-ai/multica/server/pkg/agent"
)

// The launch wiring (CHE-2512 review P1 1): runTask starts the proxy beside the
// run's broker, names its socket in the run's env, and the run's own process
// is what the fence stops. The agent here is a real executeAndDrain over a
// backend that blocks until it is stopped; only GitHub and the Multica server
// are fake.

// stoppableBackend is an agent that runs until its context ends, and may call
// into the run's env while it runs.
type stoppableBackend struct {
	running  chan struct{} // closed once the agent is running
	exited   atomic.Bool
	work     func() // what the agent's gh does while it runs, in its own process
	workDone chan struct{}
}

func (b *stoppableBackend) Execute(ctx context.Context, _ string, _ agent.ExecOptions) (*agent.Session, error) {
	msgCh := make(chan agent.Message)
	resCh := make(chan agent.Result, 1)
	if b.work != nil {
		go func() { b.work(); close(b.workDone) }() // the gh shim: a child the fence kills
	} else {
		close(b.workDone)
	}
	go func() {
		close(b.running)
		<-ctx.Done()
		b.exited.Store(true)
		close(msgCh)
		resCh <- agent.Result{Status: "aborted"}
	}()
	return &agent.Session{Messages: msgCh, Result: resCh}, nil
}

// readGitHub is GitHub's API: App token mint and revoke, and the GraphQL read.
func readGitHub(t *testing.T) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	live := map[string]bool{}
	var seq int
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			RepositoryIDs []int64           `json:"repository_ids"`
			Permissions   map[string]string `json:"permissions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		for k, v := range req.Permissions {
			if v != "read" {
				t.Errorf("minted %s:%s, want read only", k, v)
			}
		}
		mu.Lock()
		seq++
		tok := fmt.Sprintf("read-%d", seq)
		live[tok] = true
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": tok, "expires_at": time.Now().Add(time.Hour)})
	})
	mux.HandleFunc("DELETE /installation/token", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		delete(live, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ok := live[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		mu.Unlock()
		if !ok {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		body, _ := json.Marshal(wiredComment)
		_, _ = w.Write([]byte(`{"data":{"node":{"__typename":"IssueComment","id":"IC_1","url":"https://github.com/o/r/issues/1#issuecomment-1",` +
			`"body":` + string(body) + `,"lastEditedAt":null,"editor":null}}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, func() int { mu.Lock(); defer mu.Unlock(); return len(live) }
}

type launched struct {
	ctx     context.Context
	rp      *runReadProxy
	rb      *runBroker
	env     map[string]string
	backend *stoppableBackend
	trace   *wireTrace
	live    func() int
	result  chan agent.Result
}

// launch wires one elevated run the way runTask does and starts its agent.
func launch(t *testing.T, srv *httptest.Server, tr *wireTrace, live func() int, gh *httptest.Server, profile string) *launched {
	t.Helper()
	gs := gatesuite.NewGitHub(t, gatesuite.GitHubOptions{Repos: map[string]int64{"o/r": 42}})
	cfg := brokerTestConfig(t, gs)
	cfg.APIBase = gh.URL
	brokerFakeTools(t)
	multicaExe = func() (string, error) { return "/bin/true", nil }
	t.Cleanup(func() { multicaExe = os.Executable })

	env := map[string]string{}
	rb, err := applyRunBroker(sandbox.ModeOn, true, cfg, env, quietLog())
	if err != nil || rb == nil {
		t.Fatalf("applyRunBroker: rb=%v err=%v", rb, err)
	}
	t.Cleanup(rb.Close)

	c := NewClient(srv.URL)
	c.SetToken("tok")
	task := Task{ID: "task-1", IssueID: "issue-1", Epoch: 3, Profile: profile, PinnedRepo: "https://github.com/o/r.git"}
	ctx, rp, err := startRunReadProxy(context.Background(), c, task, rb, env, quietLog())
	if err != nil {
		t.Fatalf("startRunReadProxy: %v", err)
	}
	if rp != nil {
		t.Cleanup(rp.Close)
	}
	l := &launched{ctx: ctx, rp: rp, rb: rb, env: env, trace: tr, live: live, result: make(chan agent.Result, 1)}
	l.backend = &stoppableBackend{running: make(chan struct{}), workDone: make(chan struct{})}
	return l
}

// run starts the agent through the real executeAndDrain.
func (l *launched) run(t *testing.T) {
	t.Helper()
	d := newTestDaemon(t)
	go func() {
		res, _, _ := d.executeAndDrain(l.ctx, l.backend, "p", agent.ExecOptions{}, quietLog(), "task-1", "", new(atomic.Int32))
		l.result <- res
	}()
	select {
	case <-l.backend.running:
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not start")
	}
}

func TestLaunchWiresProxyBesideBrokerAndFencesTheRealRun(t *testing.T) {
	tr := &wireTrace{}
	var l *launched
	var runGoneAtEnd atomic.Bool
	srv := wireServer(t, tr, http.StatusOK, func(o *wireOpts) {
		o.onEnded = func() { runGoneAtEnd.Store(l.backend.exited.Load()) }
	})
	gh, live := readGitHub(t)
	l = launch(t, srv, tr, live, gh, "elevated")
	if l.rp == nil {
		t.Fatal("an elevated run with a broker got no read proxy")
	}
	if want := filepath.Join(filepath.Dir(l.rb.Socket), "r.sock"); l.rp.Socket != want || l.env[readproxy.SocketEnv] != want {
		t.Fatalf("socket %q env %q, want %q beside the broker socket in the run env", l.rp.Socket, l.env[readproxy.SocketEnv], want)
	}

	var res readproxy.Result
	var readErr error
	l.backend.work = func() {
		if r, err := readproxy.Read(context.Background(), l.rp.Socket, "IC_1"); err != nil || r.Full {
			t.Errorf("a read that did not ask: %+v %v, want the stub", r, err)
		}
		res, readErr = readproxy.Read(context.Background(), l.rp.Socket, "IC_1", readproxy.AskDowngrade)
	}
	l.run(t)

	select {
	case r := <-l.result:
		if r.Status != "cancelled" {
			t.Fatalf("run result %q, want the run cancelled by the fence", r.Status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the fence did not stop the run")
	}
	<-l.backend.workDone
	if readErr != nil || !res.Full || res.Body != wiredComment {
		t.Fatalf("ask: %+v %v, want full text after the handshake", res, readErr)
	}
	want := []string{"server.downgrade epoch=3", "server.launch-scopes-ended", "server.deliver"}
	if got := tr.get(); !reflect.DeepEqual(got, want) {
		t.Fatalf("server calls %v, want %v", got, want)
	}
	if !runGoneAtEnd.Load() {
		t.Fatal("launch scopes were reported ended while the run was still alive")
	}
	if !l.rp.Restricted() {
		t.Fatal("the run was not restricted locally")
	}
	l.rp.Close()
	if n := live(); n != 0 {
		t.Fatalf("%d read tokens live after the run ended", n)
	}
}

func TestLaunchFailedServerFenceKeepsLocalRestrictionAndStub(t *testing.T) {
	tr := &wireTrace{}
	srv := wireServer(t, tr, http.StatusOK, func(o *wireOpts) { o.downgradeStatus = http.StatusInternalServerError })
	gh, live := readGitHub(t)
	l := launch(t, srv, tr, live, gh, "elevated")
	if l.rp == nil {
		t.Fatal("an elevated run with a broker got no read proxy")
	}
	var res readproxy.Result
	var readErr error
	l.backend.work = func() {
		res, readErr = readproxy.Read(context.Background(), l.rp.Socket, "IC_1", readproxy.AskDowngrade)
	}
	l.run(t)
	select {
	case <-l.result:
	case <-time.After(20 * time.Second):
		t.Fatal("the run was not stopped")
	}
	<-l.backend.workDone
	if readErr != nil || res.Full || res.Body != "" {
		t.Fatalf("ask: %+v %v, want the stub", res, readErr)
	}
	if !l.rp.Restricted() {
		t.Fatal("a failed server fence must not undo the local restriction")
	}
	for _, s := range tr.get() {
		if s == "server.launch-scopes-ended" || s == "server.deliver" {
			t.Fatalf("server calls %v: nothing may be reported or delivered after a failed fence", tr.get())
		}
	}
}

func TestLaunchStartsNothingForARestrictedRun(t *testing.T) {
	tr := &wireTrace{}
	srv := wireServer(t, tr, http.StatusOK)
	gh, live := readGitHub(t)
	l := launch(t, srv, tr, live, gh, "restricted")
	if l.rp != nil || l.env[readproxy.SocketEnv] != "" {
		t.Fatalf("a restricted run got a read proxy: %+v env=%q", l.rp, l.env[readproxy.SocketEnv])
	}
}
