package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Spec describes one sandboxed run. The three writable directories and the
// ReadOnly paths are bound at their own absolute paths; nothing else from the
// host is mounted except the fixed system allowlist below.
type Spec struct {
	WorkDir  string   // read-write, becomes the working directory
	TempDir  string   // read-write, TMPDIR
	Home     string   // read-write, per-run HOME
	ReadOnly []string // extra read-only binds: toolchains, repo cache objects dir
	// Env is the complete environment of the command. Nothing is inherited.
	// HOME and TMPDIR are always set from Home and TempDir.
	Env  []string
	Argv []string
	// EgressSocket is the host Unix socket of the egress proxy. When set, the
	// run gets a loopback forwarder to it and proxy variables (see bridge.go).
	EgressSocket string
}

var (
	// systemBinds are mounted read-only when present on the host.
	systemBinds = []string{"/bin", "/sbin", "/lib", "/lib32", "/lib64"}
	// etcEntries is the filtered /etc: no resolver config, no credentials.
	etcEntries = []string{
		"/etc/alternatives", "/etc/ca-certificates", "/etc/group", "/etc/hosts", "/etc/ld.so.cache",
		"/etc/localtime", "/etc/nsswitch.conf", "/etc/os-release", "/etc/passwd", "/etc/ssl",
	}
	envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// DefaultDeny lists host paths that must never be the source of, or sit
// inside or beneath, a bind mount. It guards callers that build ReadOnly
// lists; the sandbox itself is safe because it mounts only what a Spec names.
func DefaultDeny() []string {
	deny := []string{"/var/run/docker.sock", "/run/docker.sock"}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		for _, rel := range []string{".ssh", ".config/gh", ".config/github-apps", ".claude", ".codex"} {
			deny = append(deny, filepath.Join(home, rel))
		}
	}
	return deny
}

// bind is one host path mounted at dest, or at its own path when dest is empty.
type bind struct {
	path string
	dest string
	ro   bool
}

func (b bind) target() string {
	if b.dest != "" {
		return b.dest
	}
	return b.path
}

// binds lists the spec's mounts in argument order. bridgeExe is the helper
// binary mounted when the spec has an egress socket.
func (s Spec) binds(bridgeExe string) []bind {
	bs := []bind{{path: s.WorkDir}, {path: s.Home}, {path: s.TempDir}}
	for _, p := range s.ReadOnly {
		bs = append(bs, bind{path: p, ro: true})
	}
	if s.EgressSocket != "" {
		bs = append(bs,
			bind{path: filepath.Dir(s.EgressSocket), dest: egressDir, ro: true},
			bind{path: bridgeExe, dest: bridgeMount, ro: true})
	}
	return bs
}

// Args returns the bwrap argument vector for spec, ending with "-- argv...".
// Environment values never appear in it: they travel in the process
// environment (see environ), not on a world-readable command line.
// Mounts name their sources by path; a launched run uses pinned descriptors
// instead (see pinned).
func (l *Launcher) Args(spec Spec) ([]string, error) {
	if err := l.validate(spec); err != nil {
		return nil, err
	}
	return spec.args(nil, l.bridgeExe()), nil
}

// args builds the argument vector. With pins, mount i names descriptor 3+i
// (exec.Cmd.ExtraFiles order) instead of a host path.
func (s Spec) args(pins []*os.File, bridgeExe string) []string {
	args := []string{
		"--unshare-user", "--unshare-pid", "--unshare-net", "--unshare-ipc", "--unshare-uts", "--unshare-cgroup",
		"--die-with-parent", "--new-session",
		"--ro-bind", "/usr", "/usr",
	}
	for _, p := range append(append([]string{}, systemBinds...), etcEntries...) {
		args = append(args, "--ro-bind-try", p, p)
	}
	args = append(args, "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp")
	for i, b := range s.binds(bridgeExe) {
		flag, src := "--bind", b.path
		if b.ro {
			flag = "--ro-bind"
		}
		if pins != nil {
			flag, src = flag+"-fd", strconv.Itoa(3+i)
		}
		args = append(args, flag, src, b.target())
	}
	args = append(args, "--chdir", s.WorkDir, "--")
	return append(args, s.command()...)
}

// environ is the exact environment handed to bwrap and, through it, to the
// command. A non-nil empty slice means "empty", never "inherit".
func (s Spec) environ() []string {
	env := make([]string, 0, len(s.Env)+6)
	for _, kv := range s.Env {
		if s.EgressSocket != "" && isProxyVar(kv) {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "HOME="+s.Home, "TMPDIR="+s.TempDir)
	if s.EgressSocket != "" {
		env = append(env, proxyEnv()...)
	}
	return env
}

func (l *Launcher) validate(spec Spec) error {
	if len(spec.Argv) == 0 {
		return errors.New("sandbox: empty command")
	}
	if spec.EgressSocket != "" {
		if err := l.checkEgress(spec.EgressSocket); err != nil {
			return err
		}
	}
	for _, kv := range spec.Env {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || !envName.MatchString(name) {
			return fmt.Errorf("sandbox: invalid env entry %q", envNameOnly(kv))
		}
	}
	writable := []string{spec.WorkDir, spec.Home, spec.TempDir}
	for _, b := range spec.binds(l.bridgeExe()) {
		p := b.path
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return fmt.Errorf("sandbox: path %q is not absolute and clean", p)
		}
		if bad := l.denied(p); bad != "" {
			return fmt.Errorf("sandbox: %s overlaps never-mounted path %s", p, bad)
		}
	}
	for _, b := range spec.binds(l.bridgeExe()) {
		if !b.ro {
			continue
		}
		for _, w := range writable {
			if within(w, b.path) {
				return fmt.Errorf("sandbox: read-only bind %s would expose writable dir %s", b.path, w)
			}
		}
	}
	return nil
}

// denied returns the DefaultDeny or l.Deny entry that p equals, sits inside or contains,
// judged on both the literal and the symlink-resolved path.
func (l *Launcher) denied(p string) string {
	for _, cand := range []string{p, resolve(p)} {
		for _, d := range append(DefaultDeny(), l.Deny...) {
			for _, dd := range []string{d, resolve(d)} {
				if within(cand, dd) || within(dd, cand) {
					return d
				}
			}
		}
	}
	return ""
}

// within reports whether p is dir or lies beneath it.
func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

func resolve(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// envNameOnly keeps a malformed entry's value out of error text.
func envNameOnly(kv string) string {
	name, _, _ := strings.Cut(kv, "=")
	return name
}

// pinned validates spec, then opens every mount source once and returns the
// argument vector that mounts those descriptors. A path swapped after this
// point cannot change what is mounted. The caller owns the returned files.
func (l *Launcher) pinned(spec Spec) ([]string, []*os.File, error) {
	if err := l.validate(spec); err != nil {
		return nil, nil, err
	}
	var pins []*os.File
	closeAll := func() { closeFiles(pins) }
	for _, b := range spec.binds(l.bridgeExe()) {
		f, real, err := pin(b.path)
		if err != nil {
			closeAll()
			return nil, nil, fmt.Errorf("sandbox: pin %s: %w", b.path, err)
		}
		pins = append(pins, f)
		if bad := l.denied(real); bad != "" {
			closeAll()
			return nil, nil, fmt.Errorf("sandbox: %s resolves to %s, which overlaps never-mounted path %s", b.path, real, bad)
		}
	}
	return spec.args(pins, l.bridgeExe()), pins, nil
}
