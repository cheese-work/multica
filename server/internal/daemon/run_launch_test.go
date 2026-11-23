package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/gatesuite"
	"github.com/multica-ai/multica/server/internal/daemon/sandbox"
)

// recordingLauncher is a runLauncher that leaves every command unchanged and
// records the specs it was handed.
type recordingLauncher struct {
	mu    sync.Mutex
	specs []sandbox.Spec
}

func (r *recordingLauncher) Wrap(cmd *exec.Cmd, spec sandbox.Spec) (*sandbox.Run, error) {
	r.mu.Lock()
	r.specs = append(r.specs, spec)
	r.mu.Unlock()
	return &sandbox.Run{Cmd: cmd}, nil
}
func (r *recordingLauncher) SelfCheck(context.Context) error { return nil }
func (r *recordingLauncher) wraps() int                      { r.mu.Lock(); defer r.mu.Unlock(); return len(r.specs) }

// fakeClaude is a test-created claude: it reports what it can see (pid 1 of
// its pid namespace, the broker socket variable) in its result, and touches
// $CAPTURE_FILE so a test can tell a start on the host from a start in bwrap.
const fakeClaude = `#!/bin/sh
case "$*" in *--version*) echo 9.9.9; exit 0;; esac
[ -n "$CAPTURE_FILE" ] && echo started > "$CAPTURE_FILE"
read -r p1 < /proc/1/comm
IFS= read -r _
printf '%s\n' '{"type":"system","session_id":"s1"}'
printf '{"type":"result","subtype":"success","is_error":false,"session_id":"s1","result":"pid1=%s sock=%s"}\n' "$p1" "$MULTICA_BROKER_SOCKET"
`

// launchDaemon is a Daemon able to run one fake claude task through runTask.
func launchDaemon(t *testing.T, mode sandbox.Mode, l runLauncher) (*Daemon, Task, string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("sandbox launch is Linux-only")
	}
	capture := filepath.Join(t.TempDir(), "started")
	fake := filepath.Join(t.TempDir(), "claude")
	writeTestExecutable(t, fake, []byte(fakeClaude))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	t.Cleanup(srv.Close)
	gh := gatesuite.NewGitHub(t, gatesuite.GitHubOptions{Repos: map[string]int64{"o/r": 42}})
	brokerFakeTools(t)
	logger := quietLog()
	d := &Daemon{
		client: NewClient(srv.URL), logger: logger, localPathLocks: NewLocalPathLocker(),
		workspaces: make(map[string]*workspaceState), activeEnvRoots: make(map[string]int),
		runtimeIndex: map[string]Runtime{"rt": {ID: "rt", Provider: "claude"}},
		launcher:     l,
		cfg: Config{
			DaemonID: "daemon-launch", WorkspacesRoot: t.TempDir(), AgentTimeout: 20 * time.Second,
			ServerBaseURL: srv.URL, HealthPort: 19514, SandboxMode: mode, Broker: brokerTestConfig(t, gh),
			Agents: map[string]AgentEntry{"claude": {Path: fake}},
		},
	}
	d.activeStores, d.deletingStores = make(map[string]int), make(map[string]bool)
	d.activeStoresCond = sync.NewCond(&d.activeStoresMu)
	task := Task{
		ID: "task-launch", WorkspaceID: "ws-launch", RuntimeID: "rt", IssueID: "issue-launch",
		AgentID: "agent-launch", AuthToken: "mat_launch_test",
		Agent: &AgentData{ID: "agent-launch", Name: "launch", CustomEnv: map[string]string{"CAPTURE_FILE": capture}},
	}
	return d, task, capture
}

func hostSandbox(t *testing.T) *sandbox.Launcher {
	t.Helper()
	l := &sandbox.Launcher{Mode: sandbox.ModeOn}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := l.SelfCheck(ctx); err != nil {
		t.Skipf("host cannot run the sandbox: %v", err)
	}
	return l
}

// AC 1: under on with a passing self-check, a fake claude run through runTask
// gets MULTICA_BROKER_SOCKET and starts through bwrap (it is not pid 1's
// parent: its pid namespace's pid 1 is bwrap).
func TestRunTaskOnStartsFakeClaudeThroughBwrapWithBrokerSocket(t *testing.T) {
	d, task, _ := launchDaemon(t, sandbox.ModeOn, hostSandbox(t))
	res, err := d.runTask(context.Background(), task, "claude", 0, quietLog())
	if err != nil || res.Status != "completed" {
		t.Fatalf("runTask = %+v, %v", res, err)
	}
	if !strings.Contains(res.Comment, "pid1=bwrap") {
		t.Fatalf("fake claude did not start through bwrap: %q", res.Comment)
	}
	if !strings.Contains(res.Comment, "sock=/run/multica-broker/") {
		t.Fatalf("fake claude got no broker socket: %q", res.Comment)
	}
}

// AC 2: under off and audit nothing changes: no launcher use, no broker, the
// same host launch.
func TestRunTaskOffAndAuditLaunchUnchanged(t *testing.T) {
	for _, mode := range []sandbox.Mode{sandbox.ModeOff, sandbox.ModeAudit} {
		rec := &recordingLauncher{}
		d, task, _ := launchDaemon(t, mode, rec)
		res, err := d.runTask(context.Background(), task, "claude", 0, quietLog())
		if err != nil || res.Status != "completed" {
			t.Fatalf("%s: runTask = %+v, %v", mode, res, err)
		}
		if n := rec.wraps(); n != 0 {
			t.Errorf("%s: launcher wrapped %d commands, want none", mode, n)
		}
		if strings.Contains(res.Comment, "pid1=bwrap") || !strings.HasSuffix(res.Comment, "sock=") {
			t.Errorf("%s: launch changed: %q", mode, res.Comment)
		}
	}
}

// AC 3: under on with a failing self-check the task fails with a named error
// and nothing starts on the host.
func TestRunTaskOnFailingSelfCheckFailsAndStartsNothing(t *testing.T) {
	d, task, capture := launchDaemon(t, sandbox.ModeOn, &sandbox.Launcher{Mode: sandbox.ModeOn, BwrapPath: "/nonexistent/bwrap"})
	_, err := d.runTask(context.Background(), task, "claude", 0, quietLog())
	if !errors.Is(err, sandbox.ErrSelfCheck) {
		t.Fatalf("err = %v, want sandbox.ErrSelfCheck", err)
	}
	if _, statErr := os.Stat(capture); statErr == nil {
		t.Fatal("fake claude started on the host")
	}
	if d.isolated(context.Background()) {
		t.Fatal("isolated must be false when the self-check fails")
	}
}

// AC 4: under on, a provider other than claude and codex is refused before
// start, for a normal task and for a background task.
func TestRunTaskOnRefusesUnsupportedProvider(t *testing.T) {
	for name, background := range map[string]string{"normal": "", "background": "dm_title"} {
		rec := &recordingLauncher{}
		d, task, capture := launchDaemon(t, sandbox.ModeOn, rec)
		task.BackgroundJob = background
		_, err := d.runTask(context.Background(), task, "gemini", 0, quietLog())
		if !errors.Is(err, errSandboxProviderUnsupported) {
			t.Errorf("%s: err = %v, want errSandboxProviderUnsupported", name, err)
		}
		if _, statErr := os.Stat(capture); statErr == nil || rec.wraps() != 0 {
			t.Errorf("%s: something started", name)
		}
	}
}

// isolated is derived: on, with a launcher whose self-check passes.
func TestIsolatedIsDerivedFromModeAndSelfCheck(t *testing.T) {
	ctx := context.Background()
	if (&Daemon{cfg: Config{SandboxMode: sandbox.ModeOff}}).isolated(ctx) {
		t.Error("off must not be isolated")
	}
	if (&Daemon{cfg: Config{SandboxMode: sandbox.ModeAudit}, launcher: &recordingLauncher{}}).isolated(ctx) {
		t.Error("audit must not be isolated")
	}
	if (&Daemon{cfg: Config{SandboxMode: sandbox.ModeOn}}).isolated(ctx) {
		t.Error("on without a launcher must not be isolated")
	}
	if !(&Daemon{cfg: Config{SandboxMode: sandbox.ModeOn}, launcher: &recordingLauncher{}}).isolated(ctx) {
		t.Error("on with a passing launcher must be isolated")
	}
}
