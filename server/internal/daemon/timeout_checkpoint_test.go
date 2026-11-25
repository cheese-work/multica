package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/agent"
)

// capBackend is a fake provider that behaves like a capped run: it streams the
// given tool calls, then reports `timeout` once opts.Timeout has elapsed. It
// records every Supplement call.
type capBackend struct {
	tools []agent.Message
	// supplement is the Session.Supplement behaviour; nil leaves it unset.
	supplement func(ctx context.Context, text string) error
	ready      func() bool

	mu    sync.Mutex
	calls []time.Time
	texts []string
	start time.Time
}

func (b *capBackend) Execute(ctx context.Context, _ string, opts agent.ExecOptions) (*agent.Session, error) {
	b.start = time.Now()
	messages := make(chan agent.Message, 64)
	results := make(chan agent.Result, 1)
	go func() {
		defer close(messages)
		for _, m := range b.tools {
			messages <- m
		}
		if opts.Timeout > 0 {
			select {
			case <-time.After(opts.Timeout):
			case <-ctx.Done():
			}
			results <- agent.Result{Status: "timeout", Error: "codex timed out after " + opts.Timeout.String(), SessionID: "sess-1"}
			return
		}
		results <- agent.Result{Status: "completed", Output: "done"}
	}()
	s := &agent.Session{Messages: messages, Result: results}
	if b.supplement != nil {
		s.Supplement = func(ctx context.Context, text string) error {
			b.mu.Lock()
			b.calls = append(b.calls, time.Now())
			b.texts = append(b.texts, text)
			b.mu.Unlock()
			return b.supplement(ctx, text)
		}
		s.SupplementReady = func() bool { return b.ready == nil || b.ready() }
	}
	return s, nil
}

func (b *capBackend) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.calls)
}

func checkpointTestDaemon(t *testing.T, warning time.Duration) *Daemon {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(server.Close)
	return &Daemon{
		client: NewClient(server.URL),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:    Config{AgentNearLimitWarning: warning},
	}
}

func runCapped(t *testing.T, d *Daemon, b *capBackend, cap time.Duration) *runCheckpoint {
	t.Helper()
	rc := newRunCheckpoint()
	ctx := withRunCheckpoint(t.Context(), rc)
	result, _, err := d.executeAndDrain(ctx, b, "p", agent.ExecOptions{Timeout: cap}, d.logger, "task-cp", "", new(atomic.Int32))
	if err != nil {
		t.Fatalf("executeAndDrain: %v", err)
	}
	if result.Status != "timeout" {
		t.Fatalf("status = %q, want timeout", result.Status)
	}
	return rc
}

func TestNearLimitWarningDue(t *testing.T) {
	cases := []struct {
		warning, cap time.Duration
		want         bool
	}{
		{500 * time.Millisecond, 2 * time.Second, true},
		{time.Second, 2 * time.Second, false}, // not more than half the cap
		{time.Second, 0, false},               // no cap
		{0, time.Hour, false},                 // knob off
		{10 * time.Minute, 60 * time.Minute, true},
		{10 * time.Minute, 20 * time.Minute, false},
	}
	for _, c := range cases {
		if got := nearLimitWarningDue(c.warning, c.cap); got != c.want {
			t.Errorf("nearLimitWarningDue(%s, %s) = %v, want %v", c.warning, c.cap, got, c.want)
		}
	}
}

func TestNearLimitWarningSteersOnceNearTheCap(t *testing.T) {
	t.Parallel()
	b := &capBackend{supplement: func(context.Context, string) error { return nil }}
	rc := runCapped(t, checkpointTestDaemon(t, 500*time.Millisecond), b, 2*time.Second)

	if n := b.callCount(); n != 1 {
		t.Fatalf("Supplement calls = %d, want 1", n)
	}
	at := b.calls[0].Sub(b.start)
	if at < 1300*time.Millisecond || at > 1800*time.Millisecond {
		t.Fatalf("warning at %s, want about 1.5s", at)
	}
	for _, want := range []string{"CONTINUE", "Commit", "checkpoint comment", "head commit SHA"} {
		if !strings.Contains(b.texts[0], want) {
			t.Errorf("warning text missing %q: %s", want, b.texts[0])
		}
	}
	_, _, w := rc.snapshot()
	if w.State != warningStateDelivered || w.At.IsZero() {
		t.Fatalf("warning = %+v, want delivered with a time", w)
	}
}

func TestNearLimitWarningNotSentWhenNotWithinTheSecondHalf(t *testing.T) {
	t.Parallel()
	// 1 s warning on a 2 s cap is not more than half: no steer.
	b := &capBackend{supplement: func(context.Context, string) error { return nil }}
	rc := runCapped(t, checkpointTestDaemon(t, time.Second), b, 2*time.Second)
	if n := b.callCount(); n != 0 {
		t.Fatalf("Supplement calls = %d, want 0", n)
	}
	if _, _, w := rc.snapshot(); w.State != warningStateDisabled {
		t.Fatalf("warning = %+v, want disabled", w)
	}
}

func TestNearLimitWarningNotSentWithoutACapOrWithTheKnobOff(t *testing.T) {
	t.Parallel()
	for name, warning := range map[string]time.Duration{"cap 0": 500 * time.Millisecond, "knob 0": 0} {
		b := &capBackend{supplement: func(context.Context, string) error { return nil }}
		d := checkpointTestDaemon(t, warning)
		cap := time.Duration(0)
		if name == "knob 0" {
			cap = 300 * time.Millisecond
		}
		rc := newRunCheckpoint()
		result, _, err := d.executeAndDrain(withRunCheckpoint(t.Context(), rc), b, "p", agent.ExecOptions{Timeout: cap}, d.logger, "t", "", new(atomic.Int32))
		if err != nil || result.Status == "" {
			t.Fatalf("%s: result=%+v err=%v", name, result, err)
		}
		if n := b.callCount(); n != 0 {
			t.Fatalf("%s: Supplement calls = %d, want 0", name, n)
		}
	}
}

func TestNearLimitWarningRefusedSteerIsRecordedWithTheReason(t *testing.T) {
	t.Parallel()
	b := &capBackend{supplement: func(context.Context, string) error {
		return errors.New("turn/steer rejected: no active turn")
	}}
	rc := runCapped(t, checkpointTestDaemon(t, 500*time.Millisecond), b, 2*time.Second)
	if n := b.callCount(); n != 1 {
		t.Fatalf("Supplement calls = %d, want 1 (a refusal is not retried)", n)
	}
	_, _, w := rc.snapshot()
	if w.State != warningStateNotDelivered || !strings.Contains(w.Reason, "no active turn") {
		t.Fatalf("warning = %+v, want not_delivered with the reason", w)
	}
}

func TestNearLimitWarningRetriesUntilTheSessionIsReady(t *testing.T) {
	t.Parallel()
	var ready atomic.Bool
	time.AfterFunc(900*time.Millisecond, func() { ready.Store(true) })
	b := &capBackend{ready: ready.Load, supplement: func(context.Context, string) error { return nil }}
	d := checkpointTestDaemon(t, time.Second)
	d.nearLimitRetryInterval = 50 * time.Millisecond
	rc := runCapped(t, d, b, 3*time.Second) // fires at 2 s; ready from 0.9 s
	if _, _, w := rc.snapshot(); w.State != warningStateDelivered {
		t.Fatalf("warning = %+v, want delivered", w)
	}

	// Never ready: gives up before the cap and records why.
	b2 := &capBackend{ready: func() bool { return false }, supplement: func(context.Context, string) error { return nil }}
	d2 := checkpointTestDaemon(t, 400*time.Millisecond)
	d2.nearLimitRetryInterval = 30 * time.Millisecond
	rc2 := runCapped(t, d2, b2, time.Second)
	time.Sleep(50 * time.Millisecond)
	_, _, w := rc2.snapshot()
	if w.State != warningStateNotDelivered || w.Reason != "supplement not ready" {
		t.Fatalf("warning = %+v, want not_delivered: supplement not ready", w)
	}
	if n := b2.callCount(); n != 0 {
		t.Fatalf("Supplement called %d times on a session that was never ready", n)
	}
}

func TestNearLimitWarningWithoutSupplementIsNotDelivered(t *testing.T) {
	t.Parallel()
	b := &capBackend{}
	rc := runCapped(t, checkpointTestDaemon(t, 500*time.Millisecond), b, 2*time.Second)
	if _, _, w := rc.snapshot(); w.State != warningStateNotDelivered || w.Reason == "" {
		t.Fatalf("warning = %+v, want not_delivered with a reason", w)
	}
}

func TestRunCheckpointKeepsTheLastEightToolCallsRedactedAndCut(t *testing.T) {
	t.Parallel()
	const secret = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmn"
	var tools []agent.Message
	for i := 0; i < 10; i++ {
		tools = append(tools, agent.Message{
			Type: agent.MessageToolUse, Tool: "exec_command", CallID: "c" + string(rune('a'+i)),
			Input: map[string]any{"cmd": "echo GITHUB_TOKEN=" + secret + " " + strings.Repeat("x", 300), "n": i},
		})
	}
	b := &capBackend{tools: tools}
	rc := runCapped(t, checkpointTestDaemon(t, 0), b, 300*time.Millisecond)
	actions, total, _ := rc.snapshot()
	if total != 10 || len(actions) != 8 {
		t.Fatalf("total=%d kept=%d, want 10 and 8", total, len(actions))
	}
	for i, a := range actions {
		if a.Tool != "exec_command" {
			t.Errorf("action %d tool = %q", i, a.Tool)
		}
		if n := len([]rune(a.Input)); n > timeoutCheckpointActionChars {
			t.Errorf("action %d input is %d chars, want at most %d", i, n, timeoutCheckpointActionChars)
		}
		if strings.Contains(a.Input, secret) || strings.Contains(a.Input, "ghp_") {
			t.Errorf("action %d leaks the secret: %s", i, a.Input)
		}
	}
	// Order: the first kept call is the third made (n=2), the last is n=9.
	if !strings.Contains(actions[0].Input, `"n":2`) && !strings.Contains(actions[0].Input, "xxxx") {
		t.Errorf("first kept action unexpected: %s", actions[0].Input)
	}
	for i := 1; i < len(actions); i++ {
		if actions[i].At.Before(actions[i-1].At) {
			t.Fatalf("actions out of order at %d", i)
		}
	}
}

func TestRunCheckpointOrderWithShortInputs(t *testing.T) {
	rc := newRunCheckpoint()
	for i := 0; i < 10; i++ {
		rc.recordToolCall("t", map[string]any{"n": i}, time.Unix(int64(i), 0))
	}
	actions, total, _ := rc.snapshot()
	if total != 10 || len(actions) != 8 {
		t.Fatalf("total=%d kept=%d", total, len(actions))
	}
	for i, a := range actions {
		want := `{"n":` + string(rune('2'+i)) + `}`
		if a.Input != want {
			t.Errorf("actions[%d].Input = %s, want %s", i, a.Input, want)
		}
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return runGit(t, dir, args...)
}

func TestProbeGitReposReportsBranchHeadAheadAndDirty(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	work := filepath.Join(root, "work")
	repo := filepath.Join(work, "app")
	for _, d := range []string{remote, repo} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	git(t, remote, "init", "--bare", "-b", "main")
	git(t, repo, "init", "-b", "main")
	git(t, repo, "remote", "add", "origin", remote)
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", "1")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "one")
	git(t, repo, "push", "-u", "origin", "main")
	for _, n := range []string{"b.txt", "c.txt"} {
		write(n, "x")
		git(t, repo, "add", ".")
		git(t, repo, "commit", "-m", n)
	}
	write("a.txt", "changed") // modified
	write("untracked.txt", "u")
	head := git(t, repo, "rev-parse", "HEAD")

	repos, err := probeGitRepos(t.Context(), work)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 {
		t.Fatalf("repos = %+v, want exactly the depth-1 checkout", repos)
	}
	got := repos[0]
	if got.Path != "app" || got.Branch != "main" || got.Head != head || got.Ahead != 2 || got.Dirty != 2 {
		t.Fatalf("repo = %+v, want path app, branch main, head %s, ahead 2, dirty 2", got, head)
	}
	raw, _ := json.Marshal(repos)
	if strings.Contains(string(raw), remote) {
		t.Fatalf("probe leaked a remote URL: %s", raw)
	}
}

func TestProbeGitReposWithoutUpstreamCountsCommitsNotOnAnyRemote(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo, "a"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "one")
	repos, err := probeGitRepos(t.Context(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0].Path != "." || repos[0].Branch != "feature" || repos[0].Ahead != 1 || repos[0].Dirty != 0 {
		t.Fatalf("repos = %+v", repos)
	}
}

func TestProbeGitReposMissingWorkdirFails(t *testing.T) {
	if _, err := probeGitRepos(t.Context(), filepath.Join(t.TempDir(), "gone")); err == nil {
		t.Fatal("want an error for a missing workdir")
	}
	if _, err := probeGitRepos(t.Context(), ""); err == nil {
		t.Fatal("want an error for an empty workdir")
	}
}

func TestTimeoutCheckpointProbeTimeoutStillSendsTheFail(t *testing.T) {
	var mu sync.Mutex
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/fail") {
			mu.Lock()
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(server.Close)

	d := checkpointTestDaemon(t, 0)
	d.client = NewClient(server.URL)
	d.timeoutProbeBudget = 50 * time.Millisecond
	d.gitProbe = func(ctx context.Context, _ string) ([]timeoutCheckpointRepo, error) {
		<-ctx.Done() // a hung git
		return nil, ctx.Err()
	}
	rc := newRunCheckpoint()
	rc.recordToolCall("exec_command", map[string]any{"cmd": "go test"}, time.Now())

	// Run context already cancelled, as it is when the cap fires.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	started := time.Now()
	res := d.timeoutTaskResult(ctx, "codex", agent.Result{Status: "timeout", Error: "codex timed out after 1h0m0s", SessionID: "s"}, "/work", "/root", nil, rc, d.logger)
	if time.Since(started) > 2*time.Second {
		t.Fatalf("probe was not bounded: %s", time.Since(started))
	}
	if res.TimeoutCheckpoint == nil || !strings.HasPrefix(res.TimeoutCheckpoint.Probe, "failed") {
		t.Fatalf("checkpoint = %+v, want probe: failed ...", res.TimeoutCheckpoint)
	}
	if len(res.TimeoutCheckpoint.Actions) != 1 {
		t.Fatalf("actions lost on probe failure: %+v", res.TimeoutCheckpoint)
	}

	d.reportTaskResult(t.Context(), "task-1", res, d.logger)
	mu.Lock()
	defer mu.Unlock()
	if body == nil {
		t.Fatal("FailTask was not sent")
	}
	cp, _ := body["timeout_checkpoint"].(map[string]any)
	if cp == nil || !strings.HasPrefix(cp["probe"].(string), "failed") {
		t.Fatalf("fail body timeout_checkpoint = %v", body["timeout_checkpoint"])
	}
	if body["failure_reason"] != "timeout" || body["error"] != "codex timed out after 1h0m0s" {
		t.Fatalf("error/failure_reason changed: %v / %v", body["error"], body["failure_reason"])
	}
	if strings.Contains(body["error"].(string), "go test") {
		t.Fatal("checkpoint text leaked into the task error")
	}
}

func TestTimeoutTaskResultClassifiesTheOriginalMessage(t *testing.T) {
	d := checkpointTestDaemon(t, 0)
	d.gitProbe = func(context.Context, string) ([]timeoutCheckpointRepo, error) { return nil, nil }
	original := agent.CodexSemanticInactivityMarker + " after 10m0s without agent progress (last activity: tool-result:exec_command)"
	res := d.timeoutTaskResult(t.Context(), "codex", agent.Result{Status: "timeout", Error: original}, "/w", "/r", nil, newRunCheckpoint(), d.logger)
	if res.FailureReason != FailureReasonCodexSemanticInactivity {
		t.Fatalf("failure reason = %q, want %q", res.FailureReason, FailureReasonCodexSemanticInactivity)
	}
	if res.Comment != original {
		t.Fatalf("comment = %q, want the original message unchanged", res.Comment)
	}
	plain := d.timeoutTaskResult(t.Context(), "codex", agent.Result{Status: "timeout"}, "/w", "/r", nil, nil, d.logger)
	if plain.FailureReason != "timeout" || plain.Comment != "codex timed out after "+d.cfg.agentTimeoutFor("codex").String() {
		t.Fatalf("plain timeout = %+v", plain)
	}
	if plain.TimeoutCheckpoint == nil || plain.TimeoutCheckpoint.Warning.State != warningStateDisabled {
		t.Fatalf("a nil run checkpoint must still yield a disabled-warning checkpoint: %+v", plain.TimeoutCheckpoint)
	}
}

func TestFailTaskOmitsTheCheckpointWhenAbsent(t *testing.T) {
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(server.Close)
	c := NewClient(server.URL)
	if err := c.FailTask(t.Context(), "t", "boom", "", "", "", "agent_error", false, "", "", nil); err != nil {
		t.Fatal(err)
	}
	cp := &timeoutCheckpoint{Repos: []timeoutCheckpointRepo{{Path: ".", Branch: "b", Head: strings.Repeat("a", 40)}}, Warning: timeoutCheckpointWarning{State: warningStateNotDelivered, Reason: "x"}}
	if err := c.FailTask(t.Context(), "t", "boom", "", "", "", "timeout", false, "", "", cp); err != nil {
		t.Fatal(err)
	}
	if _, ok := bodies[0]["timeout_checkpoint"]; ok {
		t.Fatal("old-path fail must not carry timeout_checkpoint")
	}
	got, _ := bodies[1]["timeout_checkpoint"].(map[string]any)
	if got == nil || got["warning"].(map[string]any)["state"] != warningStateNotDelivered {
		t.Fatalf("timeout_checkpoint = %v", bodies[1]["timeout_checkpoint"])
	}
	for _, k := range []string{"repos", "last_actions", "tool_calls", "warning"} {
		if _, ok := got[k]; !ok {
			t.Errorf("timeout_checkpoint missing %q: %v", k, got)
		}
	}
}

func TestTerminalReportPersistsTheTimeoutCheckpoint(t *testing.T) {
	cp := &timeoutCheckpoint{
		Repos:     []timeoutCheckpointRepo{{Path: ".", Branch: "b", Head: strings.Repeat("a", 40), Ahead: 2, Dirty: 1}},
		Actions:   []timeoutCheckpointAction{{Tool: "t", Input: "{}", At: time.Unix(1, 0).UTC()}},
		ToolCalls: 3, Warning: timeoutCheckpointWarning{State: warningStateDelivered, At: time.Unix(2, 0).UTC()},
	}
	rec, err := persistedTerminalReport(terminalTaskReport{kind: terminalTaskReportFail, taskID: "t1", timeoutCheckpoint: cp}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(rec)
	var back persistedTerminalTaskReport
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	report, err := back.terminalReport()
	if err != nil {
		t.Fatal(err)
	}
	if report.timeoutCheckpoint == nil || report.timeoutCheckpoint.Repos[0].Ahead != 2 || report.timeoutCheckpoint.Warning.State != warningStateDelivered {
		t.Fatalf("replayed report lost the checkpoint: %+v", report.timeoutCheckpoint)
	}
}
