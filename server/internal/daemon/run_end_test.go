package daemon

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
	d, task, _ := launchDaemon(t, sandbox.ModeOn, &recordingLauncher{})
	gh := gatesuite.NewGitHub(t, gatesuite.GitHubOptions{Repos: map[string]int64{"o/r": 42}})
	d.cfg.Broker = brokerTestConfig(t, gh)
	d.cfg.AgentTimeout = agentTimeout
	ready := filepath.Join(t.TempDir(), "ready")
	task.Agent.CustomEnv["READY_FILE"] = ready
	writeTestExecutable(t, d.cfg.Agents["claude"].Path, []byte(fakeBlockedClaude))

	b := &blockedRun{gh: gh, done: make(chan struct{})}
	go func() {
		defer close(b.done)
		b.result, b.err = d.runTask(ctx, task, "claude", 0, quietLog())
	}()
	for range 400 {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if _, err := os.Stat(ready); err != nil {
		t.Fatal("fake backend never became ready")
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
