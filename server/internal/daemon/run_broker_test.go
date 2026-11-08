package daemon

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/gatesuite"
	"github.com/multica-ai/multica/server/internal/daemon/repocache"
	"github.com/multica-ai/multica/server/internal/daemon/sandbox"
)

// CHE-4595 (A4c): the daemon starts the run broker, installs the shims and
// closes the broker at run end.

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func brokerTestConfig(t *testing.T, gh *gatesuite.GitHub) *BrokerConfig {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(gh.Key)
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return &BrokerConfig{
		AppID: gh.AppID, InstallationID: gh.InstallationID, KeyFile: keyFile, APIBase: gh.URL,
		ReadMarginSec: 7200, // every gh call replaces the previous read token
		RepoIDs:       map[string]int64{"o/r": 42},
	}
}

// fakeTools puts stand-in git and gh first on PATH and returns the dir.
func fakeTools(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"git": "#!/bin/sh\nexit 0\n", "gh": "#!/bin/sh\necho \"$GH_TOKEN\"\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

func buildMultica(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "multica")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/multica").CombinedOutput(); err != nil {
		t.Fatalf("go build multica: %v\n%s", err, out)
	}
	return bin
}

func TestRunBrokerUnchangedWhenOffOrUnconfigured(t *testing.T) {
	gh := gatesuite.NewGitHub(t, gatesuite.GitHubOptions{Repos: map[string]int64{"o/r": 42}})
	cfg := brokerTestConfig(t, gh)
	for name, c := range map[string]struct {
		mode sandbox.Mode
		cfg  *BrokerConfig
	}{
		"off": {sandbox.ModeOff, cfg}, "audit": {sandbox.ModeAudit, cfg}, "on without config": {sandbox.ModeOn, nil},
	} {
		env := map[string]string{}
		rb, err := applyRunBroker(c.mode, true, c.cfg, env, quietLog())
		if err != nil || rb != nil || len(env) != 0 {
			t.Errorf("%s: rb=%v err=%v env=%v, want no broker and an untouched env", name, rb, err, env)
		}
		rb.Close() // nil-safe
	}
}

// Without enforced run isolation every run shares the daemon UID, so a broker
// socket is reachable by other runs: no broker, no shims, env untouched. The
// daemon passes runIsolated until the sandbox launcher is on the run path.
func TestRunBrokerRefusedWithoutIsolation(t *testing.T) {
	gh := gatesuite.NewGitHub(t, gatesuite.GitHubOptions{Repos: map[string]int64{"o/r": 42}})
	env := map[string]string{}
	rb, err := applyRunBroker(sandbox.ModeOn, false, brokerTestConfig(t, gh), env, quietLog())
	if err != nil || rb != nil || len(env) != 0 {
		t.Fatalf("rb=%v err=%v env=%v, want no broker and an untouched env", rb, err, env)
	}
	if runIsolated {
		t.Fatal("runIsolated must stay false until runs go through sandbox.Launcher with the broker socket")
	}
}

// A fake agent runs gh through the shims the daemon installed. The first read
// token is revoked by the refresh; run end revokes the rest. The broker starts
// at the first checkout, not before.
func TestRunBrokerFakeAgentGHRefreshAndRevoke(t *testing.T) {
	gh := gatesuite.NewGitHub(t, gatesuite.GitHubOptions{Repos: map[string]int64{"o/r": 42}})
	cfg := brokerTestConfig(t, gh)
	fakeTools(t)
	mc := buildMultica(t)
	multicaExe = func() (string, error) { return mc, nil }
	t.Cleanup(func() { multicaExe = os.Executable })

	env := map[string]string{}
	rb, err := applyRunBroker(sandbox.ModeOn, true, cfg, env, quietLog())
	if err != nil || rb == nil {
		t.Fatalf("applyRunBroker: rb=%v err=%v", rb, err)
	}
	defer rb.Close()
	if !strings.HasPrefix(env["PATH"], rb.ShimDir+":") || env["MULTICA_BROKER_SOCKET"] != rb.Socket {
		t.Fatalf("env must lead PATH with the shims and name the socket: PATH=%q socket=%q", env["PATH"], env["MULTICA_BROKER_SOCKET"])
	}
	if _, err := os.Stat(rb.Socket); err == nil {
		t.Fatal("broker must not listen before the first checkout")
	}
	if err := rb.start(brokerCheckout{TaskID: "t1", RepoURL: "https://github.com/o/r.git", Branch: "agent/x/t1",
		Base: strings.Repeat("a", 40), Objects: t.TempDir(), Bases: []string{"main"},
		Policy: func(context.Context) ([]string, error) { return []string{"AGENTS.md"}, nil }}); err != nil {
		t.Fatalf("start: %v", err)
	}

	agent := filepath.Join(t.TempDir(), "agent")
	os.WriteFile(agent, []byte("#!/bin/sh\ngh pr view 1\ngh pr view 2\n"), 0o755)
	cmd := exec.Command(agent)
	cmd.Env = []string{"HOME=" + t.TempDir(), "GH_TOKEN=ambient", "GITHUB_TOKEN=ambient"}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("agent: %v\n%s", err, out)
	}
	seen := strings.Fields(string(out))
	if len(seen) != 2 || seen[0] == seen[1] || strings.Contains(string(out), "ambient") {
		t.Fatalf("gh saw %d tokens, want two distinct broker tokens and never the ambient one", len(seen))
	}
	if seen[0] != gh.Token(1) || seen[1] != gh.Token(2) {
		t.Fatal("gh must see mint 1 then mint 2")
	}
	if u := gh.Unrevoked(); len(u) != 1 || u[0] != 2 {
		t.Fatalf("after the refresh only mint 2 may be live, live=%v", u)
	}
	rb.Close()
	gh.AssertClean(t, 42, gatesuite.ReadPerms)
	if _, err := os.Stat(rb.Socket); err == nil {
		t.Fatal("run end must remove the socket")
	}
}

func TestRunBrokerStartRefusals(t *testing.T) {
	gh := gatesuite.NewGitHub(t, gatesuite.GitHubOptions{Repos: map[string]int64{"o/r": 42}})
	fakeTools(t)
	rb, err := applyRunBroker(sandbox.ModeOn, true, brokerTestConfig(t, gh), map[string]string{}, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	defer rb.Close()
	base := brokerCheckout{TaskID: "t1", Branch: "agent/x/t1", Base: strings.Repeat("a", 40), Objects: t.TempDir(),
		Bases: []string{"main"}, Policy: func(context.Context) ([]string, error) { return []string{"x"}, nil }}
	for name, url := range map[string]string{"repository with no ID": "https://github.com/o/other.git", "not github": "https://example.com/o/r.git"} {
		c := base
		c.RepoURL = url
		if err := rb.start(c); err == nil {
			t.Errorf("%s: start must fail", name)
		}
	}
	if _, err := os.Stat(rb.Socket); err == nil {
		t.Error("a refused start must leave no listener")
	}
}

type fixedBare struct {
	repoCacheBackend
	bare string
}

func (f fixedBare) BarePath(string, string) string { return f.bare }

// The checkout hook builds the pin from the real checkout: default branch from
// the cache, base from the worktree HEAD, objects from the cache.
func TestStartRunBrokerFromCheckout(t *testing.T) {
	gh := gatesuite.NewGitHub(t, gatesuite.GitHubOptions{Repos: map[string]int64{"o/r": 42}})
	realGit := "/usr/bin/git" // not PATH's git: a host wrapper may need credentials
	if _, err := os.Stat(realGit); err != nil {
		t.Skip("no /usr/bin/git")
	}
	fakeTools(t)
	root := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(realGit, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@x", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@x")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	bare, work := filepath.Join(root, "cache.git"), filepath.Join(root, "work")
	run(root, "init", "--bare", "-b", "main", bare)
	run(root, "clone", bare, work)
	run(work, "commit", "--allow-empty", "-m", "base")
	run(work, "push", "origin", "main")
	run(work, "checkout", "-b", "agent/x/t1")

	rb, err := applyRunBroker(sandbox.ModeOn, true, brokerTestConfig(t, gh), map[string]string{}, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	defer rb.Close()
	t.Setenv("PATH", filepath.Dir(realGit)) // gitOut finds the real git
	d := &Daemon{logger: quietLog(), client: &Client{}, repoCache: fixedBare{bare: bare}}
	task := activeRepoCheckoutTask{TaskID: "t1", Broker: rb}
	req := repoCheckoutRequest{URL: "https://github.com/o/r.git", WorkspaceID: "w", TaskID: "t1"}

	d.startRunBroker(task, req, &repocache.WorktreeResult{Path: work, BranchName: ""})
	if _, err := os.Stat(rb.Socket); err == nil {
		t.Fatal("a checkout with no branch must not start the broker")
	}
	// A checkout kept for local work on a branch this task does not own must
	// grant no authority over that branch.
	d.startRunBroker(task, req, &repocache.WorktreeResult{Path: work, BranchName: "someone/elses", Kept: repocache.KeptLocalWork})
	if _, err := os.Stat(rb.Socket); err == nil {
		t.Fatal("a kept foreign branch must not start the broker")
	}
	d.startRunBroker(task, req, &repocache.WorktreeResult{Path: work, BranchName: "agent/x/t1", Kept: repocache.KeptTaskBranch})
	if _, err := os.Stat(rb.Socket); err != nil {
		t.Fatalf("broker must listen after the checkout: %v", err)
	}
	d.startRunBroker(activeRepoCheckoutTask{TaskID: "t1"}, req, &repocache.WorktreeResult{Path: work, BranchName: "agent/x/t1"}) // no broker: no-op
}
