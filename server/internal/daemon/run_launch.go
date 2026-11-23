package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"

	"github.com/multica-ai/multica/server/internal/daemon/sandbox"
)

// runLauncher is the part of *sandbox.Launcher runTask uses under enforcement
// on. An interface so a test can record the specs it is handed.
type runLauncher interface {
	Wrap(cmd *exec.Cmd, spec sandbox.Spec) (*sandbox.Run, error)
	SelfCheck(ctx context.Context) error
}

// errSandboxProviderUnsupported: under enforcement on only claude and codex
// have a tested sandbox launch; any other provider is refused before start.
var errSandboxProviderUnsupported = errors.New("sandbox: enforcement on supports only claude and codex runs")

// newRunLauncher builds the daemon's one launcher (CHE-6504). Only enforcement
// on has one; off and audit keep the plain launch.
func newRunLauncher(mode sandbox.Mode, logger *slog.Logger) runLauncher {
	if mode != sandbox.ModeOn {
		return nil
	}
	exe, _ := os.Executable() // serves the in-sandbox bridge (sandbox.BridgeHelperArg)
	return &sandbox.Launcher{Mode: sandbox.ModeOn, BridgeExe: exe, Logger: logger}
}

// isolated reports whether runs sit behind enforced filesystem isolation that
// exposes only the run's own sockets: enforcement on and a launcher whose
// self-check passes. It gates the per-run broker (applyRunBroker).
func (d *Daemon) isolated(ctx context.Context) bool {
	return d.cfg.SandboxMode == sandbox.ModeOn && d.launcher != nil && d.launcher.SelfCheck(ctx) == nil
}

// requireSandboxLaunch stops a task before it starts when enforcement is on and
// it cannot start in the sandbox: an unsupported provider, or a host whose
// self-check fails. Off and audit never refuse. It runs before the background
// and script early returns in runTask so those paths are covered too.
func (d *Daemon) requireSandboxLaunch(ctx context.Context, provider string) error {
	if d.cfg.SandboxMode != sandbox.ModeOn {
		return nil
	}
	if provider != "claude" && provider != "codex" {
		return fmt.Errorf("%w (provider %q)", errSandboxProviderUnsupported, provider)
	}
	if d.launcher == nil {
		return fmt.Errorf("%w: no launcher", sandbox.ErrSelfCheck)
	}
	return d.launcher.SelfCheck(ctx)
}

// runLaunch is one run's sandbox launch: the egress proxy, the daemon API
// socket and the hook that wraps each process the run's backend starts. Close
// ends them all when the run ends.
type runLaunch struct {
	hook *runLaunchHook

	egress  *runEgress
	stopAPI func()
	apiDir  string
}

// runLaunchHook is the agent.LaunchHook over Launcher.Wrap. The backend may
// start several processes per run (turns, probes); each gets its own cgroup,
// released by Close.
type runLaunchHook struct {
	l    runLauncher
	spec sandbox.Spec

	mu   sync.Mutex
	runs []*sandbox.Run
}

func (h *runLaunchHook) Wrap(cmd *exec.Cmd) error {
	run, err := h.l.Wrap(cmd, h.spec)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.runs = append(h.runs, run)
	h.mu.Unlock()
	return nil
}

func (h *runLaunchHook) close(log *slog.Logger) {
	h.mu.Lock()
	runs := h.runs
	h.runs = nil
	h.mu.Unlock()
	for _, r := range runs {
		if err := r.Close(); err != nil {
			log.Warn("sandbox: run close", "error", err)
		}
	}
}

// Close is safe on nil and on repeat.
func (rl *runLaunch) Close(log *slog.Logger) {
	if rl == nil {
		return
	}
	if rl.hook != nil {
		rl.hook.close(log)
	}
	rl.egress.Close()
	if rl.stopAPI != nil {
		rl.stopAPI()
		rl.stopAPI = nil
		_ = os.RemoveAll(rl.apiDir)
	}
}

// runLaunchInput is what startRunLaunch reads from runTask.
type runLaunchInput struct {
	Task         Task
	Provider     string
	WorkDir      string
	TempDir      string
	Env          map[string]string // the agent env, final
	AgentExe     string
	GuardShimDir string
	Broker       *runBroker
	DaemonExe    string
}

// startRunLaunch prepares one run's launch under enforcement on: the S4 egress
// proxy, the S5 daemon API socket, the S3 spec, and the hook over Launcher.Wrap.
// Off and audit return nil, nil: the hook stays nil and the launch is unchanged.
// A failure stops the run before the agent starts.
func (d *Daemon) startRunLaunch(in runLaunchInput, log *slog.Logger) (*runLaunch, error) {
	if d.cfg.SandboxMode != sandbox.ModeOn {
		return nil, nil
	}
	if d.launcher == nil {
		return nil, fmt.Errorf("%w: no launcher", sandbox.ErrSelfCheck)
	}
	eg, err := startRunEgress(d.cfg.SandboxMode, egressTrustedHosts(d.cfg.ServerBaseURL, in.Provider), log)
	if err != nil {
		return nil, fmt.Errorf("start egress proxy: %w", err)
	}
	rl := &runLaunch{egress: eg}
	fail := func(err error) (*runLaunch, error) { rl.Close(log); return nil, err }

	// Short path: a Unix socket path is limited to about 100 bytes.
	if rl.apiDir, err = os.MkdirTemp("", "mrd-"); err != nil {
		return fail(fmt.Errorf("daemon API dir: %w", err))
	}
	sock, stop, err := d.startRunAPI(rl.apiDir)
	if err != nil {
		_ = os.RemoveAll(rl.apiDir)
		rl.apiDir = ""
		return fail(fmt.Errorf("start daemon API: %w", err))
	}
	rl.stopAPI = stop

	repos := make([]runSandboxRepo, 0, len(in.Task.Repos))
	for _, r := range in.Task.Repos {
		repo := runSandboxRepo{URL: r.URL}
		if d.repoCache != nil {
			repo.Bare = d.repoCache.Lookup(in.Task.WorkspaceID, r.URL)
		}
		repos = append(repos, repo)
	}
	si := runSandboxInput{
		WorkDir: in.WorkDir, TempDir: in.TempDir, Home: in.Env["HOME"], Env: in.Env,
		AgentExe: in.AgentExe, GuardShimDir: in.GuardShimDir, EgressSocket: eg.Socket,
		DaemonExe: in.DaemonExe, Repos: repos,
	}
	if in.Broker != nil {
		si.BrokerSocket, si.BrokerShimDir = in.Broker.Socket, in.Broker.ShimDir
	}
	spec, notes := buildRunSandboxSpec(si)
	for _, n := range notes {
		log.Info(n)
	}
	spec.DaemonSocket, spec.DaemonPort = sock, in.Env["MULTICA_DAEMON_PORT"]
	rl.hook = &runLaunchHook{l: d.launcher, spec: spec}
	return rl, nil
}
