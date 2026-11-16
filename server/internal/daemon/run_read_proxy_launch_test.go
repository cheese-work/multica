package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/gatesuite"
	"github.com/multica-ai/multica/server/internal/daemon/processtree"
	"github.com/multica-ai/multica/server/internal/daemon/readproxy"
	"github.com/multica-ai/multica/server/internal/daemon/sandbox"
	"github.com/multica-ai/multica/server/pkg/agent"
)

// The launch wiring (CHE-2512 review P1 1): runTask starts the proxy beside the
// run's broker, names its socket in the run's env, and the run's own process
// is what the fence leaves running. The agent here is a real executeAndDrain
// over a backend that starts a real child process (the requesting gh): the
// child reads through the socket in its env and the run's result is what the
// child printed. Only GitHub and the Multica server are fake.

// childRead is what the run's gh does: one plain read, then one that asks for
// the downgrade, printed as JSON. It runs as a real child process: the test
// binary re-executed, because the fence must not kill the process that asked.
func TestRunReadProxyChild(t *testing.T) {
	sock := os.Getenv("RRP_CHILD_SOCK")
	if sock == "" {
		t.Skip("child helper")
	}
	ctx := context.Background()
	plain, perr := readproxy.Read(ctx, sock, "IC_1")
	asked, aerr := readproxy.Read(ctx, sock, "IC_1", readproxy.AskDowngrade)
	out, _ := json.Marshal(map[string]any{"plain_full": plain.Full, "plain_err": fmt.Sprint(perr), "full": asked.Full, "body": asked.Body, "sha": asked.SHA256, "err": fmt.Sprint(aerr)})
	fmt.Println(string(out))
	os.Exit(0)
}

type childOut struct {
	PlainFull bool   `json:"plain_full"`
	Full      bool   `json:"full"`
	Body      string `json:"body"`
	SHA       string `json:"sha"`
	Err       string `json:"err"`
}

// childBackend is the agent: it runs the requesting gh as its child and
// reports what the child printed, the way a real agent reports a tool result.
type childBackend struct{ sock string }

func (b *childBackend) Execute(ctx context.Context, _ string, _ agent.ExecOptions) (*agent.Session, error) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunReadProxyChild$")
	cmd.Env = append(os.Environ(), "RRP_CHILD_SOCK="+b.sock)
	var out bytes.Buffer
	cmd.Stdout = &out
	msgCh := make(chan agent.Message)
	resCh := make(chan agent.Result, 1)
	go func() {
		err := processtree.Run(ctx, cmd, time.Second)
		close(msgCh)
		switch {
		case ctx.Err() != nil:
			resCh <- agent.Result{Status: "cancelled", Output: out.String()}
		case err != nil:
			resCh <- agent.Result{Status: "failed", Error: err.Error(), Output: out.String()}
		default:
			resCh <- agent.Result{Status: "completed", Output: out.String()}
		}
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
	rp    *runReadProxy
	rb    *runBroker
	env   map[string]string
	trace *wireTrace
	live  func() int
}

// launch wires one run the way runTask does.
func launch(t *testing.T, srv *httptest.Server, tr *wireTrace, live func() int, gh *httptest.Server, profile string, scopes ...string) *launched {
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
	task := Task{ID: "task-1", IssueID: "issue-1", Epoch: 3, Profile: profile, Scopes: scopes, PinnedRepo: "https://github.com/o/r.git"}
	rp, err := startRunReadProxy(c, task, rb, env, quietLog())
	if err != nil {
		t.Fatalf("startRunReadProxy: %v", err)
	}
	if rp != nil {
		t.Cleanup(rp.Close)
	}
	return &launched{rp: rp, rb: rb, env: env, trace: tr, live: live}
}

// run is the agent's whole turn through the real executeAndDrain; it returns
// the result and what the requesting child printed.
func (l *launched) run(t *testing.T) (agent.Result, childOut) {
	t.Helper()
	if l.rp == nil {
		t.Fatal("an elevated run with a broker got no read proxy")
	}
	d := newTestDaemon(t)
	type ran struct {
		res agent.Result
		err error
	}
	done := make(chan ran, 1)
	go func() {
		res, _, err := d.executeAndDrain(context.Background(), &childBackend{sock: l.env[readproxy.SocketEnv]}, "p", agent.ExecOptions{}, quietLog(), "task-1", "", new(atomic.Int32))
		done <- ran{res, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("executeAndDrain: %v", r.err)
		}
		var out childOut
		if err := json.Unmarshal([]byte(strings.TrimSpace(r.res.Output)), &out); err != nil && r.res.Status == "completed" {
			t.Fatalf("child output %q: %v", r.res.Output, err)
		}
		return r.res, out
	case <-time.After(60 * time.Second):
		t.Fatal("the run did not finish")
		return agent.Result{}, childOut{}
	}
}

func TestLaunchRequestingChildReceivesBytesAfterTheFence(t *testing.T) {
	tr := &wireTrace{}
	srv := wireServer(t, tr, http.StatusOK)
	gh, live := readGitHub(t)
	l := launch(t, srv, tr, live, gh, "elevated", "repo_write:1234")
	if l.rp == nil {
		t.Fatal("an elevated run with a broker got no read proxy")
	}
	if want := filepath.Join(filepath.Dir(l.rb.Socket), "r.sock"); l.rp.Socket != want || l.env[readproxy.SocketEnv] != want {
		t.Fatalf("socket %q env %q, want %q beside the broker socket in the run env", l.rp.Socket, l.env[readproxy.SocketEnv], want)
	}

	res, out := l.run(t)
	if res.Status != "completed" {
		t.Fatalf("run %q, want the run still alive to receive the bytes", res.Status)
	}
	if out.PlainFull || !out.Full || out.Body != wiredComment {
		t.Fatalf("child saw %+v, want the stub without an ask and the full text after the handshake", out)
	}
	want := []string{"server.downgrade epoch=3", "server.deliver"}
	if got := tr.get(); !reflect.DeepEqual(got, want) {
		t.Fatalf("server calls %v, want %v: no launch scope, so nothing to end", got, want)
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
	l := launch(t, srv, tr, live, gh, "elevated", "repo_write:1234")
	res, out := l.run(t)
	if res.Status != "completed" || out.Full || out.Body != "" || out.SHA == "" {
		t.Fatalf("run %q child %+v, want the child to get the stub", res.Status, out)
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

// A task holding docker or device: scopes cannot be fenced without ending the
// processes that hold them, and those include the one that asked. The proxy
// refuses the downgrade up front: no fence starts, the run stays as it was, and
// the child gets the stub.
func TestLaunchRefusesDowngradeWhileLaunchScopesAreHeld(t *testing.T) {
	for _, scope := range []string{"docker", "device:usb0"} {
		t.Run(scope, func(t *testing.T) {
			tr := &wireTrace{}
			srv := wireServer(t, tr, http.StatusOK)
			gh, live := readGitHub(t)
			l := launch(t, srv, tr, live, gh, "elevated", "repo_write:1234", scope)
			res, out := l.run(t)
			if res.Status != "completed" || out.Full || out.Body != "" || out.SHA == "" {
				t.Fatalf("run %q child %+v, want the stub and a run left alone", res.Status, out)
			}
			if got := tr.get(); len(got) != 0 {
				t.Fatalf("server calls %v, want none: no fence may start", got)
			}
		})
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
