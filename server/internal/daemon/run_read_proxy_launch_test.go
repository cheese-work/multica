package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
type childBackend struct {
	sock    string
	session string // the session id the run reports, so a successor can resume it
}

func (b *childBackend) Execute(ctx context.Context, _ string, _ agent.ExecOptions) (*agent.Session, error) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunReadProxyChild$")
	cmd.Env = append(os.Environ(), "RRP_CHILD_SOCK="+b.sock)
	var out bytes.Buffer
	cmd.Stdout = &out
	msgCh := make(chan agent.Message)
	resCh := make(chan agent.Result, 1)
	go func() {
		// The backend reveals its session as soon as it starts, like a real one.
		select {
		case msgCh <- agent.Message{Type: agent.MessageStatus, SessionID: b.session}:
		case <-ctx.Done():
		}
		err := processtree.Run(ctx, cmd, time.Second)
		close(msgCh)
		switch {
		case ctx.Err() != nil:
			resCh <- agent.Result{Status: "cancelled", Output: out.String(), SessionID: b.session}
		case err != nil:
			resCh <- agent.Result{Status: "failed", Error: err.Error(), Output: out.String(), SessionID: b.session}
		default:
			resCh <- agent.Result{Status: "completed", Output: out.String(), SessionID: b.session}
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

// launchRestricted is the restricted launcher the next launch() wires into the
// run. Production has none (restrictedSuccessorLaunch is nil), so a test that
// needs one sets it.
var launchRestricted restrictedLaunch

func withRestricted(t *testing.T, r restrictedLaunch) {
	t.Helper()
	launchRestricted = r
	t.Cleanup(func() { launchRestricted = nil })
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
	rp, err := startRunReadProxy(c, task, rb, env, launchRestricted, quietLog())
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
		res, _, err := d.executeWithDowngrade(context.Background(), l.rp, &childBackend{sock: l.env[readproxy.SocketEnv]}, "p", agent.ExecOptions{}, quietLog(), "task-1", "", new(atomic.Int32))
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

// successorBackend is the agent across turns: turn 1 is the requesting child,
// every later turn records what it was given and answers.
//
// The backend holds a docker launch grant, the way the original run's env and
// process do. Every turn after the first records whether it could still use
// it (launches); a restricted successor is built without it.
type successorBackend struct {
	sock     string
	mu       sync.Mutex
	prompts  []string
	resumes  []string
	launches []bool
}

// try is a turn attempting the launch capability the run held before the fence.
func (b *successorBackend) try(holdsGrant bool) {
	b.mu.Lock()
	b.launches = append(b.launches, holdsGrant)
	b.mu.Unlock()
}

// restrictedSuccessor is the launcher a real restricted launch provides: the
// successor runs a backend that does not hold the grant.
func (b *successorBackend) restrictedSuccessor(_ agent.Backend, opts agent.ExecOptions) (agent.Backend, agent.ExecOptions, error) {
	return &restrictedBackend{parent: b}, opts, nil
}

// restrictedBackend is the successor's backend: it records its turn on the
// parent and its launch attempt fails, because it was built without the grant.
type restrictedBackend struct{ parent *successorBackend }

func (r *restrictedBackend) Execute(ctx context.Context, prompt string, opts agent.ExecOptions) (*agent.Session, error) {
	r.parent.mu.Lock()
	r.parent.prompts = append(r.parent.prompts, prompt)
	r.parent.resumes = append(r.parent.resumes, opts.ResumeSessionID)
	r.parent.mu.Unlock()
	r.parent.try(false)
	return finishedSession("continued"), nil
}

func finishedSession(out string) *agent.Session {
	msgCh := make(chan agent.Message)
	close(msgCh)
	resCh := make(chan agent.Result, 1)
	resCh <- agent.Result{Status: "completed", Output: out, SessionID: "sess-2"}
	return &agent.Session{Messages: msgCh, Result: resCh}
}

func (b *successorBackend) Execute(ctx context.Context, prompt string, opts agent.ExecOptions) (*agent.Session, error) {
	b.mu.Lock()
	b.prompts = append(b.prompts, prompt)
	b.resumes = append(b.resumes, opts.ResumeSessionID)
	n := len(b.prompts)
	b.mu.Unlock()
	if n == 1 {
		return (&childBackend{sock: b.sock, session: "sess-1"}).Execute(ctx, prompt, opts)
	}
	b.try(true) // the same backend as the requester: it still holds the grant
	return finishedSession("continued"), nil
}

func (b *successorBackend) turns() (prompts, resumes []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.prompts...), append([]string(nil), b.resumes...)
}

func (l *launched) runTurns(t *testing.T, b agent.Backend) (agent.Result, error) {
	t.Helper()
	d := newTestDaemon(t)
	type ran struct {
		res agent.Result
		err error
	}
	done := make(chan ran, 1)
	go func() {
		res, _, err := d.executeWithDowngrade(context.Background(), l.rp, b, "first prompt", agent.ExecOptions{}, quietLog(), "task-1", "", new(atomic.Int32))
		done <- ran{res, err}
	}()
	select {
	case r := <-done:
		return r.res, r.err
	case <-time.After(60 * time.Second):
		t.Fatal("the task did not finish")
		return agent.Result{}, nil
	}
}

// A task holding docker or device: scopes cannot be fenced without ending the
// processes that hold them, and those include the one that asked. The
// requester dies with them; the bytes go to a restricted successor turn that
// resumes the same session, after the fence and the launch-scopes report.
func TestLaunchScopeTaskDeliversToARestrictedSuccessor(t *testing.T) {
	for _, scope := range []string{"docker", "device:usb0"} {
		t.Run(scope, func(t *testing.T) {
			tr := &wireTrace{}
			srv := wireServer(t, tr, http.StatusOK)
			gh, live := readGitHub(t)
			b := &successorBackend{}
			withRestricted(t, b.restrictedSuccessor)
			l := launch(t, srv, tr, live, gh, "elevated", "repo_write:1234", scope)
			b.sock = l.env[readproxy.SocketEnv]
			res, err := l.runTurns(t, b)
			if err != nil {
				t.Fatalf("task: %v", err)
			}
			if res.Status != "completed" || res.Output != "continued" {
				t.Fatalf("result %q %q, want the successor's completed answer", res.Status, res.Output)
			}
			prompts, resumes := b.turns()
			if len(prompts) != 2 {
				t.Fatalf("%d turns, want the requester then one successor", len(prompts))
			}
			if strings.Contains(prompts[0], wiredComment) {
				t.Fatal("the first turn's prompt carries the withheld text")
			}
			if !strings.Contains(prompts[1], wiredComment) {
				t.Fatalf("successor prompt %q lacks the delivered text", prompts[1])
			}
			if resumes[1] != "sess-1" {
				t.Fatalf("successor resumes %q, want the requester's session sess-1", resumes[1])
			}
			b.mu.Lock()
			launches := append([]bool(nil), b.launches...)
			b.mu.Unlock()
			if !reflect.DeepEqual(launches, []bool{false}) {
				t.Fatalf("successor launch attempts %v, want one, and denied: it received the text, so it must not hold the launch grant", launches)
			}
			want := []string{"server.downgrade epoch=3", "server.launch-scopes-ended", "server.deliver"}
			if got := tr.get(); !reflect.DeepEqual(got, want) {
				t.Fatalf("server calls %v, want %v", got, want)
			}
			if !l.rp.Restricted() {
				t.Fatal("the run was not restricted locally")
			}
		})
	}
}

// A failed server fence still stops the run (the fence is fail closed: the
// run is restricted, drained and stopped, and nothing is reported ended). No
// successor is started and the task fails loudly instead of reporting a
// cancelled run.
func TestLaunchScopeFailedFenceFailsTheTaskAndReportsNothing(t *testing.T) {
	tr := &wireTrace{}
	srv := wireServer(t, tr, http.StatusOK, func(o *wireOpts) { o.downgradeStatus = http.StatusInternalServerError })
	gh, live := readGitHub(t)
	b := &successorBackend{}
	withRestricted(t, b.restrictedSuccessor)
	l := launch(t, srv, tr, live, gh, "elevated", "repo_write:1234", "docker")
	b.sock = l.env[readproxy.SocketEnv]
	_, err := l.runTurns(t, b)
	if err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("err %v, want a downgrade failure naming the stopped run", err)
	}
	if prompts, _ := b.turns(); len(prompts) != 1 {
		t.Fatalf("%d turns, want no successor", len(prompts))
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

// The run was stopped and the delivery then failed: there is no recipient and
// no way back, so the task fails loudly instead of reporting a cancelled run.
func TestLaunchScopeDeliveryFailureFailsTheTask(t *testing.T) {
	tr := &wireTrace{}
	srv := wireServer(t, tr, http.StatusInternalServerError)
	gh, live := readGitHub(t)
	b := &successorBackend{}
	withRestricted(t, b.restrictedSuccessor)
	l := launch(t, srv, tr, live, gh, "elevated", "repo_write:1234", "docker")
	b.sock = l.env[readproxy.SocketEnv]
	_, err := l.runTurns(t, b)
	if err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("err %v, want a downgrade failure naming the stopped run", err)
	}
	if prompts, _ := b.turns(); len(prompts) != 1 {
		t.Fatalf("%d turns, want no successor after a failed delivery", len(prompts))
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

// No restricted launcher exists in production yet, so a launch-scope task
// cannot get a successor that lacks its grants. The handshake is then refused
// before it starts: the proxy keeps the stub, nothing is fenced, tainted or
// delivered, and the requester keeps its run. Nothing reaches a process that
// still holds docker or device access.
func TestLaunchScopeWithoutARestrictedLauncherDeliversNothing(t *testing.T) {
	for _, scope := range []string{"docker", "device:usb0"} {
		t.Run(scope, func(t *testing.T) {
			tr := &wireTrace{}
			srv := wireServer(t, tr, http.StatusOK)
			gh, live := readGitHub(t)
			l := launch(t, srv, tr, live, gh, "elevated", "repo_write:1234", scope)
			res, out := l.run(t)
			if res.Status != "completed" || out.Full || out.Body != "" || out.SHA == "" {
				t.Fatalf("run %q child %+v, want the requester to keep running with the stub", res.Status, out)
			}
			if got := tr.get(); len(got) != 0 {
				t.Fatalf("server calls %v, want none: no fence, no taint, no delivery", got)
			}
			if l.rp.Restricted() {
				t.Fatal("the run was fenced although nothing was delivered")
			}
		})
	}
}

// The launcher cannot build a restricted successor after the fence: the text
// was withheld from the dead requester, so the task fails and the original
// backend, which still holds the grants, never receives it.
func TestLaunchScopeRestrictedLaunchFailureFailsTheTask(t *testing.T) {
	tr := &wireTrace{}
	srv := wireServer(t, tr, http.StatusOK)
	gh, live := readGitHub(t)
	withRestricted(t, func(agent.Backend, agent.ExecOptions) (agent.Backend, agent.ExecOptions, error) {
		return nil, agent.ExecOptions{}, errors.New("no sandbox")
	})
	l := launch(t, srv, tr, live, gh, "elevated", "repo_write:1234", "docker")
	b := &successorBackend{sock: l.env[readproxy.SocketEnv]}
	_, err := l.runTurns(t, b)
	if err == nil || !strings.Contains(err.Error(), "restricted") {
		t.Fatalf("err %v, want a failure naming the restricted launch", err)
	}
	if prompts, _ := b.turns(); len(prompts) != 1 {
		t.Fatalf("%d turns, want the requester only: the unrestricted backend must not get the text", len(prompts))
	}
}
