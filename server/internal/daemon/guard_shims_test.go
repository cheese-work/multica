package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
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

	"github.com/multica-ai/multica/server/internal/daemon/broker"
	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

var (
	guardBinOnce sync.Once
	guardBinDir  string
	guardBinPath string
	guardBinErr  error
	// guardBuildHome is the HOME before TestMain isolated it.
	guardBuildHome string
)

// guardMulticaBin builds the real multica CLI once per test binary, so the
// shims run the real `multica guard exec`. TestMain removes it.
func guardMulticaBin(t *testing.T) string {
	t.Helper()
	guardBinOnce.Do(func() {
		guardBinDir, guardBinErr = os.MkdirTemp("", "multica-guard-bin-")
		if guardBinErr != nil {
			return
		}
		guardBinPath = filepath.Join(guardBinDir, "multica")
		build := exec.Command("go", "build", "-o", guardBinPath, "github.com/multica-ai/multica/server/cmd/multica")
		build.Env = append(os.Environ(), "HOME="+guardBuildHome)
		out, err := build.CombinedOutput()
		if err != nil {
			guardBinErr = fmt.Errorf("go build multica: %w\n%s", err, out)
		}
	})
	if guardBinErr != nil {
		t.Fatal(guardBinErr)
	}
	return guardBinPath
}

func removeGuardMulticaBin() {
	if guardBinDir != "" {
		_ = os.RemoveAll(guardBinDir)
	}
}

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

			dir, err := frontGuardShims(agentEnv, taskTemp, guardMulticaBin(t))
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

// CI-wait calls are refused in the shim of every runtime, with the wakeup line
// and without reaching the real tool; plain calls pass (CHE-5206, plugin 2).
// Aliases and `gh api` GraphQL mutations get no way around it. The shims do not
// depend on the provider, so each runtime's PATH setup is the same call.
func TestGuardShimsDenyCIWaitForEveryRuntime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell shims are POSIX only")
	}
	for _, provider := range []string{"claude", "codex", "gemini", "hermes"} {
		t.Run(provider, func(t *testing.T) {
			tools := t.TempDir()
			writeTool(t, tools, "git", `echo real-git "$@"`)
			writeTool(t, tools, "sleep", `echo real-sleep "$@"`)
			// `gh sw` is a shell alias: the real gh runs gh again from its PATH.
			writeTool(t, tools, "gh", `case "$1 $2" in
"alias list") printf 'cw:\tpr checks $1 --watch\n' ;;
"sw ") exec gh run watch 7 ;;
*) echo real-gh "$@" ;;
esac`)
			t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
			agentEnv := map[string]string{"PATH": tools + string(os.PathListSeparator) + "/usr/bin:/bin"}
			if _, err := frontGuardShims(agentEnv, t.TempDir(), guardMulticaBin(t)); err != nil {
				t.Fatal(err)
			}
			delete(agentEnv, "HOME")

			for _, script := range []string{
				"gh run watch 123",
				"gh pr checks 5 --watch",
				"gh -R o/r run watch 1",
				"gh cw 5",
				"gh sw",
				"sleep 30",
				"sleep 10 10",
				"sleep infinity",
				"gh api graphql -f query='mutation { markPullRequestReadyForReview(input:{}) { clientMutationId } }'",
				"gh api repos/o/r/pulls -X POST -f title=x",
			} {
				got := runInEnv(t, agentEnv, script+" 2>&1; echo exit=$?")
				if !strings.HasPrefix(got, "guard: ") || !strings.HasSuffix(got, "\nexit=2") || strings.Contains(got, "real-") {
					t.Errorf("%s: want one guard line and exit 2 without reaching the real tool, got %q", script, got)
				}
			}
			for script, want := range map[string]string{
				"gh pr checks 5":                  "real-gh pr checks 5",
				"gh pr checks 5 --required":       "real-gh pr checks 5 --required",
				"gh run view 7":                   "real-gh run view 7",
				"gh api repos/o/r/actions/runs/7": "real-gh api repos/o/r/actions/runs/7",
				"sleep 5":                         "real-sleep 5",
				"sleep 0.5":                       "real-sleep 0.5",
				"git status":                      "real-git status",
			} {
				if got := runInEnv(t, agentEnv, script+" 2>&1"); got != want {
					t.Errorf("%s = %q, want %q", script, got, want)
				}
			}
			if got := runInEnv(t, agentEnv, "gh run watch 1 2>&1; echo exit=$?"); !strings.Contains(got, "--until-gh-run") {
				t.Errorf("run watch line lacks the wakeup condition: %q", got)
			}
		})
	}
}

// Without a multica binary the guarded shims hand straight over, like git's.
func TestGuardShimsWithoutMulticaBinaryPassThrough(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell shims are POSIX only")
	}
	tools := t.TempDir()
	writeTool(t, tools, "gh", `echo real-gh "$@"`)
	agentEnv := map[string]string{"PATH": tools}
	if _, err := frontGuardShims(agentEnv, t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	if got := runInEnv(t, agentEnv, "gh run watch 1"); got != "real-gh run watch 1" {
		t.Fatalf("got %q", got)
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
	dir, err := frontGuardShims(agentEnv, t.TempDir(), guardMulticaBin(t))
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
	shims, err := frontGuardShims(agentEnv, t.TempDir(), guardMulticaBin(t))
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
	dir, err := frontGuardShims(env, t.TempDir(), guardMulticaBin(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(env["PATH"], dir+string(os.PathListSeparator)) || !strings.Contains(env["PATH"], real) {
		t.Fatalf("PATH = %q, want %q first, then the daemon PATH containing %q", env["PATH"], dir, real)
	}
}

// runTask launches each runtime's agent process with the guard shims first on
// its PATH: a test-created agent records what `git` and `gh` resolve to from
// inside the process runTask starts (CHE-5203). No installed CLI is run.
func TestRunTaskLaunchesAgentWithGuardShims(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script agent fixture is POSIX-only")
	}
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			real := fakeTools(t)
			origResolve := resolveSelfExecutable
			resolveSelfExecutable = func() (string, error) { return guardMulticaBin(t), nil }
			t.Cleanup(func() { resolveSelfExecutable = origResolve })
			seen := filepath.Join(t.TempDir(), "resolved")
			fake := filepath.Join(t.TempDir(), provider+"-fixture")
			writeTestExecutable(t, fake, []byte("#!/bin/sh\n"+
				"case \"$*\" in *--version*) echo '9.9.9'; exit 0;; esac\n"+
				"{ command -v git; command -v gh; command -v sleep; git --version; gh run watch 1 2>&1; echo exit=$?; sleep 30 2>&1; echo exit=$?; } > '"+seen+"'\n"+
				"exit 0\n"))
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{}`))
			}))
			defer srv.Close()
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			d := &Daemon{
				client: NewClient(srv.URL), logger: logger,
				workspaces: make(map[string]*workspaceState), activeEnvRoots: make(map[string]int),
				runtimeIndex: map[string]Runtime{"test-rt": {ID: "test-rt", Provider: provider}},
				cfg: Config{
					WorkspacesRoot: t.TempDir(), AgentTimeout: 5 * time.Second, ServerBaseURL: srv.URL,
					Agents: map[string]AgentEntry{provider: {Path: fake}},
				},
			}
			task := Task{
				ID: "test-task", WorkspaceID: "test-ws", RuntimeID: "test-rt", IssueID: "test-issue",
				AgentID: "test-agent", AuthToken: "mat_test_guard",
				Agent: &AgentData{ID: "test-agent", Name: "test"},
			}
			d.activeStores, d.deletingStores = make(map[string]int), make(map[string]bool)
			d.activeStoresCond = sync.NewCond(&d.activeStoresMu)
			// The fixture speaks no agent protocol, so the run may end failed; only
			// the environment it started with matters here.
			_, _ = d.runTask(context.Background(), task, provider, 0, logger)

			out, err := os.ReadFile(seen)
			if err != nil {
				t.Fatalf("%s agent never started with a recorded PATH: %v", provider, err)
			}
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			if len(lines) != 8 {
				t.Fatalf("resolution output = %q", out)
			}
			for i, name := range guardShimNames {
				got := lines[i]
				if filepath.Base(got) != name || filepath.Base(filepath.Dir(got)) != "guard-bin" {
					t.Fatalf("%s resolves to %q in the %s run, want a guard-bin shim (real is in %s)", name, got, provider, real)
				}
			}
			if lines[3] != "real-git --version" {
				t.Fatalf("git shim did not reach the real git: %q", lines[3])
			}
			// The same shims refuse a CI wait for this runtime's agent process.
			for i, want := range []string{"gh run watch", "sleep of 20s"} {
				line, exit := lines[4+2*i], lines[5+2*i]
				if !strings.HasPrefix(line, "guard: ") || !strings.Contains(line, want) || exit != "exit=2" {
					t.Fatalf("%s run: %q then %q, want a guard line about %q and exit=2", provider, line, exit, want)
				}
			}
		})
	}
}
