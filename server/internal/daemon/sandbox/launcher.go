package sandbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
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

// Wrap turns a prepared cmd into a sandboxed run in place, so the caller keeps
// its own pipes, cancel and wait handling. Start cmd itself, call Started right
// after, and Close after Wait. In ModeOn a refusal leaves cmd untouched and
// there is no unsandboxed fallback. cmd.Env becomes the complete environment
// (nil means empty, never inherited) and an empty cmd.Dir runs in spec.WorkDir.
func (l *Launcher) Wrap(cmd *exec.Cmd, spec Spec) (*Run, error) {
	spec.Env, spec.Argv = cmd.Env, append([]string{cmd.Path}, cmd.Args[min(1, len(cmd.Args)):]...)
	if cmd.Dir != "" {
		spec.chdir = cmd.Dir
	}
	switch l.Mode {
	case ModeOn:
		if err := checkWrap(cmd, spec); err != nil {
			return nil, err
		}
		if err := l.SelfCheck(context.Background()); err != nil {
			return nil, err
		}
		return l.wrap(cmd, spec)
	case ModeAudit:
		if err := checkWrap(cmd, spec); err != nil {
			l.logger().Warn("sandbox audit: would refuse", "reason", err)
		}
		l.audit(context.Background(), spec)
	}
	return &Run{Cmd: cmd, log: l.logger(), began: time.Now()}, nil
}

// checkWrap refuses commands whose own settings the sandbox cannot honour.
func checkWrap(cmd *exec.Cmd, spec Spec) error {
	if cmd.Err != nil {
		return cmd.Err
	}
	if len(cmd.ExtraFiles) > 0 {
		return errors.New("sandbox: command already has ExtraFiles")
	}
	if cmd.Dir != "" && (!filepath.IsAbs(cmd.Dir) || !within(filepath.Clean(cmd.Dir), spec.WorkDir)) {
		return fmt.Errorf("sandbox: command Dir %q is outside the work dir", cmd.Dir)
	}
	return nil
}

func (l *Launcher) wrap(cmd *exec.Cmd, spec Spec) (*Run, error) {
	path, args, pins, sc, err := l.prepare(spec)
	if err != nil {
		return nil, err
	}
	cmd.Path, cmd.Args = path, append([]string{path}, args...)
	cmd.Env, cmd.Dir, cmd.ExtraFiles = spec.environ(), "", pins
	sc.attach(cmd)
	if orig := cmd.Cancel; orig != nil {
		cmd.Cancel = func() error { _ = sc.kill(); return orig() }
	}
	return &Run{Cmd: cmd, scope: sc, pins: pins, log: l.logger(), began: time.Now()}, nil
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
	began := time.Now()
	path, args, pins, sc, err := l.prepare(spec)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = spec.environ()
	cmd.ExtraFiles = pins
	sc.attach(cmd)
	return &Run{Cmd: cmd, scope: sc, pins: pins, log: l.logger(), began: began}, nil
}

// prepare resolves bwrap, pins the mount sources and creates the run cgroup.
func (l *Launcher) prepare(spec Spec) (path string, args []string, pins []*os.File, sc *scope, err error) {
	if path, err = exec.LookPath(l.bwrap()); err != nil {
		return
	}
	if args, pins, err = l.pinned(spec); err != nil {
		return
	}
	if sc, err = newScope(l.CgroupRoot); err != nil {
		closeFiles(pins)
	}
	return
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
		Argv: []string{"/bin/true"}, NoDaemonAPI: true,
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
	r.Started()
	r.log.Debug("sandbox start", "elapsed", time.Since(r.began), "sandboxed", r.scope != nil, "error", err)
	return err
}

// Started releases the mount pins and the cgroup handle. Call it right after
// Cmd.Start when the caller starts the command itself (see Wrap).
func (r *Run) Started() {
	closeFiles(r.pins)
	r.pins = nil
	if r.scope != nil {
		r.scope.releaseFD()
	}
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
