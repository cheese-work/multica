package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/multica-ai/multica/server/internal/daemon/repocache"
	"github.com/multica-ai/multica/server/internal/daemon/sandbox"
)

// runSandboxRepo is one task repository. Bare is its cache path, "" when the
// repository is not in the cache at launch.
type runSandboxRepo struct{ URL, Bare string }

// runSandboxInput is everything buildRunSandboxSpec needs; it reads no daemon
// state. The daemon API socket is not here: sandbox.Spec has no field for it
// (CHE-6489 S5 owns that bind).
type runSandboxInput struct {
	WorkDir, TempDir, Home string
	Env                    map[string]string // the agent env
	AgentExe               string            // agent executable, resolved from PATH by the caller
	GuardShimDir           string
	BrokerSocket           string // runBrk.Socket, "" without a broker
	BrokerShimDir          string
	EgressSocket           string
	DaemonExe              string
	Repos                  []runSandboxRepo
	ExtraReadOnly          []string // e.g. the scripts directory (S8)
}

// buildRunSandboxSpec builds the per-run sandbox.Spec (CHE-2216 §3.2). It
// returns the spec and task-log notes for what it left out on purpose. Argv is
// the caller's. A read-only candidate that is missing, not absolute, overlaps a
// never-mounted path (sandbox.DefaultDeny, judged literally and through
// symlinks) or would contain a writable directory is dropped, never bound.
func buildRunSandboxSpec(in runSandboxInput) (sandbox.Spec, []string) {
	var notes []string
	cands := []string{agentInstallRoot(in.AgentExe)}
	cands = append(cands, filepath.SplitList(in.Env["PATH"])...)
	cands = append(cands, in.GuardShimDir, in.BrokerShimDir, in.DaemonExe)
	for _, r := range in.Repos {
		if r.Bare == "" {
			notes = append(notes, fmt.Sprintf("sandbox: repository %s is not in the cache at launch; its objects directory is not bound", r.URL))
			continue
		}
		cands = append(cands, repocache.ObjectsDir(r.Bare))
	}
	cands = append(cands, in.ExtraReadOnly...)

	writable := []string{in.WorkDir, in.TempDir, in.Home}
	deny := sandbox.DefaultDeny()
	var ro []string
	seen := map[string]bool{}
	for _, p := range cands {
		if p == "" {
			continue
		}
		p = filepath.Clean(p) // Launcher.validate rejects an unclean path
		if !filepath.IsAbs(p) || seen[p] || !exists(p) {
			continue
		}
		seen[p] = true
		if overlapsAny(p, deny) || containsAny(p, writable) {
			continue
		}
		ro = append(ro, p)
	}

	env := make([]string, 0, len(in.Env))
	for k, v := range in.Env {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	return sandbox.Spec{
		WorkDir: in.WorkDir, TempDir: in.TempDir, Home: in.Home,
		ReadOnly: ro, Env: env,
		EgressSocket: in.EgressSocket, BrokerSocket: in.BrokerSocket,
	}, notes
}

// agentInstallRoot is the nearest ancestor of the symlink-resolved executable
// that holds a package.json (a node install), else the executable's directory.
func agentInstallRoot(exe string) string {
	if exe == "" {
		return ""
	}
	rp, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return ""
	}
	for d := filepath.Dir(rp); d != "/" && d != "."; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "package.json")); err == nil {
			return d
		}
	}
	return filepath.Dir(rp)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func under(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

func resolved(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// overlapsAny: p equals, sits inside or contains a deny entry, on the literal
// or the symlink-resolved path (same rule as sandbox.Launcher).
func overlapsAny(p string, deny []string) bool {
	for _, c := range []string{p, resolved(p)} {
		for _, d := range deny {
			for _, dd := range []string{d, resolved(d)} {
				if under(c, dd) || under(dd, c) {
					return true
				}
			}
		}
	}
	return false
}

// containsAny: a read-only bind of p would expose one of the writable dirs.
func containsAny(p string, writable []string) bool {
	for _, w := range writable {
		if under(w, p) {
			return true
		}
	}
	return false
}
