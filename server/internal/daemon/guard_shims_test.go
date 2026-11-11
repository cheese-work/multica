package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

// fakeTools puts fake `git` and `gh` first on the daemon's PATH and returns
// their directory.
func fakeTools(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range guardShimNames {
		script := "#!/bin/sh\necho real-" + name + " \"$@\"\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
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

			dir, err := installGuardShims(taskTemp)
			if err != nil {
				t.Fatalf("installGuardShims: %v", err)
			}
			withGuardShims(agentEnv, dir)

			for _, name := range guardShimNames {
				cmd := exec.Command("sh", "-c", "command -v "+name)
				cmd.Env = []string{"PATH=" + agentEnv["PATH"]}
				out, err := cmd.Output()
				if err != nil {
					t.Fatalf("command -v %s: %v", name, err)
				}
				got := strings.TrimSpace(string(out))
				if want := filepath.Join(dir, name); got != want {
					t.Fatalf("%s resolves to %q, want shim %q (real is in %s)", name, got, want, real)
				}
				run := exec.Command(name, "--version")
				run.Env = []string{"PATH=" + agentEnv["PATH"]}
				ran, err := run.CombinedOutput()
				if err != nil || strings.TrimSpace(string(ran)) != "real-"+name+" --version" {
					t.Fatalf("%s shim did not pass through to the real tool: %q %v", name, ran, err)
				}
			}
			if !strings.HasPrefix(dir, taskTemp) {
				t.Fatalf("shim dir %q must live under the task temp dir %q so the run cleans it up", dir, taskTemp)
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

// A host without gh still gets a git shim, and a missing tool is not an error.
func TestGuardShimsSkipMissingTool(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell shims are POSIX only")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	shims, err := installGuardShims(t.TempDir())
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

// withGuardShims adds the directory when PATH is unset in agentEnv too.
func TestWithGuardShimsFallsBackToDaemonPATH(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	env := map[string]string{}
	withGuardShims(env, "/shims")
	if want := "/shims" + string(os.PathListSeparator) + "/usr/bin"; env["PATH"] != want {
		t.Fatalf("PATH = %q, want %q", env["PATH"], want)
	}
}
