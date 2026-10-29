package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

// Args returns the bwrap argument vector for spec, ending with "-- argv...".
// Environment values never appear in it: they travel in the process
// environment (see environ), not on a world-readable command line.
func (l *Launcher) Args(spec Spec) ([]string, error) {
	if err := l.validate(spec); err != nil {
		return nil, err
	}
	args := []string{
		"--unshare-user", "--unshare-pid", "--unshare-net", "--unshare-ipc", "--unshare-uts", "--unshare-cgroup",
		"--die-with-parent", "--new-session",
		"--ro-bind", "/usr", "/usr",
	}
	for _, p := range append(append([]string{}, systemBinds...), etcEntries...) {
		args = append(args, "--ro-bind-try", p, p)
	}
	args = append(args, "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp")
	for _, p := range []string{spec.WorkDir, spec.Home, spec.TempDir} {
		args = append(args, "--bind", p, p)
	}
	for _, p := range spec.ReadOnly {
		args = append(args, "--ro-bind", p, p)
	}
	args = append(args, "--chdir", spec.WorkDir, "--")
	return append(args, spec.Argv...), nil
}

// environ is the exact environment handed to bwrap and, through it, to the
// command. A non-nil empty slice means "empty", never "inherit".
func (s Spec) environ() []string {
	env := append(make([]string, 0, len(s.Env)+2), s.Env...)
	return append(env, "HOME="+s.Home, "TMPDIR="+s.TempDir)
}

func (l *Launcher) validate(spec Spec) error {
	if len(spec.Argv) == 0 {
		return errors.New("sandbox: empty command")
	}
	for _, kv := range spec.Env {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || !envName.MatchString(name) {
			return fmt.Errorf("sandbox: invalid env entry %q", envNameOnly(kv))
		}
	}
	writable := []string{spec.WorkDir, spec.Home, spec.TempDir}
	for _, p := range append(append([]string{}, writable...), spec.ReadOnly...) {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return fmt.Errorf("sandbox: path %q is not absolute and clean", p)
		}
		if bad := l.denied(p); bad != "" {
			return fmt.Errorf("sandbox: %s overlaps never-mounted path %s", p, bad)
		}
	}
	for _, p := range spec.ReadOnly {
		for _, w := range writable {
			if within(w, p) {
				return fmt.Errorf("sandbox: read-only bind %s would expose writable dir %s", p, w)
			}
		}
	}
	return nil
}

// denied returns the deny entry that p equals, sits inside or contains,
// judged on both the literal and the symlink-resolved path.
func (l *Launcher) denied(p string) string {
	for _, cand := range []string{p, resolve(p)} {
		for _, d := range l.Deny {
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
