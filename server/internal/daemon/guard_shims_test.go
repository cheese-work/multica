package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/broker"
	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

// writeTool writes an executable shell script and returns its path.
func writeTool(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeTools puts fake `git` and `gh` first on the daemon's PATH and returns
// their directory.
func fakeTools(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range guardShimNames {
		writeTool(t, dir, name, "echo real-"+name+` "$@"`)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// runInEnv runs `/bin/sh -c script` (absolute, so the test process PATH plays no part) with exactly the agent's env map, so sh resolves
// every tool from the agent PATH and runs the resolved shim.
func runInEnv(t *testing.T, env map[string]string, script string) string {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%q: %v\n%s", script, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Every run, whatever its runtime, resolves gh and git to the shim, and the
// shim still reaches the real tool (CHE-5203). No broker is configured here.
func TestGuardShimsResolveFirstForEveryRuntime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell shims are POSIX only")
	}
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			real := fakeTools(t)
			taskTemp := t.TempDir()
			agentEnv := map[string]string{"PATH": "/opt/multica/bin" + string(os.PathListSeparator) + os.Getenv("PATH")}

			dir, err := frontGuardShims(agentEnv, taskTemp)
			if err != nil {
				t.Fatalf("frontGuardShims: %v", err)
			}
			if !strings.HasPrefix(dir, taskTemp) {
				t.Fatalf("shim dir %q must live under the task temp dir %q so the run cleans it up", dir, taskTemp)
			}

			for _, name := range guardShimNames {
				if got, want := runInEnv(t, agentEnv, "command -v "+name), filepath.Join(dir, name); got != want {
					t.Fatalf("%s resolves to %q, want shim %q (real is in %s)", name, got, want, real)
				}
				if got := runInEnv(t, agentEnv, name+" --version"); got != "real-"+name+" --version" {
					t.Fatalf("%s shim did not pass through to the real tool: %q", name, got)
				}
			}

			if provider == "codex" {
				allow := execenv.CodexShellEnvAllowlist(nil, agentEnv, nil)
				found := false
				for _, k := range allow {
					found = found || strings.EqualFold(k, "PATH")
				}
				if !found {
					t.Fatalf("codex shell allowlist %v drops PATH, so the shim would not reach its tool calls", allow)
				}
			}
		})
	}
}

// With credential-broker shims already on the agent PATH, the guard shim runs
// first and hands over to them: git push still reaches the broker and gh still
// gets only the broker's token. The guard never skips the broker shims.
func TestGuardShimsComposeWithBrokerShims(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell shims are POSIX only")
	}
	tools := t.TempDir()
	multica := writeTool(t, tools, "multica", `case "$1 $2" in
"git read-token") echo broker-token ;;
"git push") echo broker-push ;;
*) exit 9 ;;
esac`)
	realGit := writeTool(t, tools, "realgit", `echo real-git "$@"`)
	realGH := writeTool(t, tools, "realgh", `echo gh-token=$GH_TOKEN`)
	brokerDir := t.TempDir()
	if err := broker.InstallShims(brokerDir, realGit, realGH, multica); err != nil {
		t.Fatalf("InstallShims: %v", err)
	}
	// Host tools further down the PATH must not be reached.
	hostDir := t.TempDir()
	for _, name := range guardShimNames {
		writeTool(t, hostDir, name, "echo host-"+name+" bypassed the broker; exit 1")
	}
	t.Setenv("PATH", hostDir)

	agentEnv := map[string]string{
		"PATH":         brokerDir + string(os.PathListSeparator) + hostDir,
		"GH_TOKEN":     "ambient",
		"GITHUB_TOKEN": "ambient",
	}
	dir, err := frontGuardShims(agentEnv, t.TempDir())
	if err != nil {
		t.Fatalf("frontGuardShims: %v", err)
	}
	if first := filepath.SplitList(agentEnv["PATH"])[0]; first != dir {
		t.Fatalf("PATH starts with %q, want the guard dir %q", first, dir)
	}
	for _, name := range guardShimNames {
		if got, want := runInEnv(t, agentEnv, "command -v "+name), filepath.Join(dir, name); got != want {
			t.Fatalf("%s resolves to %q, want the guard shim %q", name, got, want)
		}
	}
	if got := runInEnv(t, agentEnv, "git push origin main"); got != "broker-push" {
		t.Fatalf("git push = %q, want it routed to the broker (broker-push)", got)
	}
	if got := runInEnv(t, agentEnv, "git status"); got != "real-git status" {
		t.Fatalf("git status = %q, want the real git behind the broker shim", got)
	}
	if got := runInEnv(t, agentEnv, "gh pr view 1"); got != "gh-token=broker-token" {
		t.Fatalf("gh saw %q, want only the broker token", got)
	}
}

// A host without gh still gets a git shim, and a missing tool is not an error.
func TestGuardShimsSkipMissingTool(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell shims are POSIX only")
	}
	dir := t.TempDir()
	writeTool(t, dir, "git", "exit 0")
	agentEnv := map[string]string{"PATH": dir}
	shims, err := frontGuardShims(agentEnv, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(shims, "git")); err != nil {
		t.Fatalf("git shim missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(shims, "gh")); err == nil {
		t.Fatal("gh shim written although no real gh exists")
	}
}

// With no PATH in agentEnv the guard falls back to the daemon's PATH.
func TestGuardShimsFallBackToDaemonPATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell shims are POSIX only")
	}
	real := fakeTools(t)
	env := map[string]string{}
	dir, err := frontGuardShims(env, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(env["PATH"], dir+string(os.PathListSeparator)) || !strings.Contains(env["PATH"], real) {
		t.Fatalf("PATH = %q, want %q first, then the daemon PATH containing %q", env["PATH"], dir, real)
	}
}
