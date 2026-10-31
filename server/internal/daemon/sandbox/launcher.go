package sandbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ErrSelfCheck means the host cannot run the sandbox; ModeOn refuses to start.
var ErrSelfCheck = errors.New("sandbox self-check failed")

// Launcher builds sandboxed commands according to Mode.
type Launcher struct {
	Mode       Mode
	BwrapPath  string   // default: "bwrap" from PATH
	CgroupRoot string   // default: the daemon's own cgroup
	Deny       []string // extra never-mounted paths, on top of DefaultDeny (always enforced)
	BridgeExe  string   // binary that serves BridgeHelperArg; default: this executable
	Logger     *slog.Logger

	mu      sync.Mutex
	checked bool
}

// Run is a command prepared by Command. Set up pipes on Cmd, then Start,
// Cmd.Wait and Close, in that order.
type Run struct {
	Cmd   *exec.Cmd
	scope *scope
	pins  []*os.File // validated mount sources, held open until Start returns
	log   *slog.Logger
	began time.Time
}

func (l *Launcher) logger() *slog.Logger {
	if l.Logger != nil {
		return l.Logger
	}
	return slog.Default()
}

func (l *Launcher) bwrap() string {
	if l.BwrapPath != "" {
		return l.BwrapPath
	}
	return "bwrap"
}

// Command prepares spec.Argv. In ModeOn the command runs in the sandbox or
// not at all; there is no fallback to an unsandboxed run.
func (l *Launcher) Command(ctx context.Context, spec Spec) (*Run, error) {
	switch l.Mode {
	case ModeOn:
		if err := l.SelfCheck(ctx); err != nil {
			return nil, err
		}
		return l.build(ctx, spec)
	case ModeAudit:
		l.audit(ctx, spec)
	}
	if len(spec.Argv) == 0 {
		return nil, errors.New("sandbox: empty command")
	}
	cmd := exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.WorkDir
	return &Run{Cmd: cmd, log: l.logger(), began: time.Now()}, nil
}

func (l *Launcher) audit(ctx context.Context, spec Spec) {
	if err := l.SelfCheck(ctx); err != nil {
		l.logger().Warn("sandbox audit: would refuse", "reason", err)
	}
	if _, err := l.Args(spec); err != nil {
		l.logger().Warn("sandbox audit: would refuse", "reason", err)
	}
}

func (l *Launcher) build(ctx context.Context, spec Spec) (*Run, error) {
	path, err := exec.LookPath(l.bwrap())
	if err != nil {
		return nil, err
	}
	began := time.Now()
	args, pins, err := l.pinned(spec)
	if err != nil {
		return nil, err
	}
	sc, err := newScope(l.CgroupRoot)
	if err != nil {
		closeFiles(pins)
		return nil, err
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = spec.environ()
	cmd.ExtraFiles = pins
	sc.attach(cmd)
	return &Run{Cmd: cmd, scope: sc, pins: pins, log: l.logger(), began: began}, nil
}

func closeFiles(fs []*os.File) {
	for _, f := range fs {
		f.Close()
	}
}

// SelfCheck proves this host can start a sandbox: user, pid, net, ipc, uts
// and cgroup namespaces plus a writable run cgroup. A success is cached.
func (l *Launcher) SelfCheck(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.checked {
		return nil
	}
	dir, err := os.MkdirTemp("", "multica-sandbox-check-")
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSelfCheck, err)
	}
	defer os.RemoveAll(dir)
	run, err := l.build(ctx, Spec{
		WorkDir: dir + "/work", TempDir: dir + "/tmp", Home: dir + "/home",
		Argv: []string{"/bin/true"},
	}.mkdirs())
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSelfCheck, err)
	}
	defer run.Close()
	out, err := run.Cmd.CombinedOutput()
	run.scope.releaseFD()
	if err != nil {
		return fmt.Errorf("%w: %w: %s", ErrSelfCheck, err, strings.TrimSpace(string(out)))
	}
	l.checked = true
	return nil
}

// mkdirs creates the writable dirs of a self-check spec.
func (s Spec) mkdirs() Spec {
	for _, d := range []string{s.WorkDir, s.TempDir, s.Home} {
		_ = os.MkdirAll(d, 0o755)
	}
	return s
}

// Start starts the command and logs how long the launcher took.
func (r *Run) Start() error {
	err := r.Cmd.Start()
	closeFiles(r.pins)
	r.pins = nil
	if r.scope != nil {
		r.scope.releaseFD()
	}
	r.log.Debug("sandbox start", "elapsed", time.Since(r.began), "sandboxed", r.scope != nil, "error", err)
	return err
}

// Kill SIGKILLs everything the run started, including re-parented children.
func (r *Run) Kill() error {
	if r.scope == nil {
		if r.Cmd.Process == nil {
			return nil
		}
		return r.Cmd.Process.Kill()
	}
	return r.scope.kill()
}

// Close removes the run's cgroup after killing leftovers. Call it after Wait.
func (r *Run) Close() error {
	closeFiles(r.pins)
	r.pins = nil
	if r.scope == nil {
		return nil
	}
	return r.scope.close()
}
