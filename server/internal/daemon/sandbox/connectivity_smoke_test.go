//go:build agentintegration && linux

package sandbox

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/egress"
)

// Real-CLI smoke of the A6 entry gate (CHE-2216 slice A7c). It runs the
// installed Claude and Codex CLIs inside the sandbox with the real egress
// proxy and checks that the CLI's traffic shows in the proxy log. It needs the
// dev team leader's authorization under the repo's agentintegration rule and
// spends one tiny model call per provider. Procedure: docs/engineering/sandbox-connectivity-smoke.md.
//
// Without MULTICA_RUN_REAL_AGENT_SMOKE=1 the test skips. With it, every
// prerequisite is required: a missing CLI, credential or sandbox fails the
// run, and the result line below names both providers, so a pass can never
// mean "one provider ran" or "nothing ran".
//
//	(cd server && MULTICA_RUN_REAL_AGENT_SMOKE=1 go test -tags=agentintegration \
//	  ./internal/daemon/sandbox -run TestSmokeRealCLI -count=1 -v)
//
// MULTICA_SMOKE_READONLY adds colon-separated read-only toolchain paths (for
// example the node install a CLI script needs).
func TestSmokeRealCLIThroughIsolatedPath(t *testing.T) {
	if os.Getenv("MULTICA_RUN_REAL_AGENT_SMOKE") != "1" {
		t.Skip("set MULTICA_RUN_REAL_AGENT_SMOKE=1 to run the real-CLI sandbox smoke")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	l := bridgeLauncher()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := l.SelfCheck(ctx); err != nil {
		t.Fatalf("smoke was authorized but the host cannot run the sandbox: %v", err)
	}
	result := map[string]string{}
	for _, p := range []struct {
		cli        string
		args       []string
		credential string // under the user's home, copied read-only into the run HOME
		hosts      []string
	}{
		{"claude", []string{"-p", "Reply with the single word OK."}, ".claude/.credentials.json", []string{"api.anthropic.com"}},
		{"codex", []string{"exec", "--skip-git-repo-check", "Reply with the single word OK."}, ".codex/auth.json", []string{"chatgpt.com", "api.openai.com"}},
	} {
		t.Run(p.cli, func(t *testing.T) {
			smokeCLI(t, l, home, p.cli, p.args, p.credential, p.hosts)
			result[p.cli] = "PASS"
		})
	}
	if result["claude"] != "PASS" || result["codex"] != "PASS" {
		t.Fatalf("smoke needs both providers: %v", result)
	}
	t.Logf("SMOKE RESULT claude=%s codex=%s", result["claude"], result["codex"])
}

func smokeCLI(t *testing.T, l *Launcher, home, cli string, args []string, credential string, hosts []string) {
	bin, err := exec.LookPath(cli)
	if err != nil {
		t.Fatalf("%s is not installed: %v", cli, err)
	}
	if real, err := filepath.EvalSymlinks(bin); err == nil {
		bin = real
	}
	cred, err := os.ReadFile(filepath.Join(home, credential))
	if err != nil {
		t.Fatalf("no %s credential on this host: %v", cli, err)
	}
	ly := newLayout(t)

	proxy := &egress.Proxy{Trusted: hosts}
	spec := proxySpec(t, ly, proxy)
	// The credential copy is a read-only bind over its own path in the run HOME.
	spec.ReadOnly = append(spec.ReadOnly, putCredential(t, ly, credential, string(cred)), filepath.Dir(bin))
	for _, p := range strings.Split(os.Getenv("MULTICA_SMOKE_READONLY"), ":") {
		if p != "" {
			spec.ReadOnly = append(spec.ReadOnly, p)
		}
	}

	// The run must not be able to change the copy (mode bits are not enough).
	probe := spec
	probe.Argv = []string{"/bin/sh", "-c", `chmod 600 "$HOME/` + credential + `" 2>/dev/null && echo CHMOD-WORKED; echo x >> "$HOME/` + credential + `" 2>/dev/null && echo APPEND-WORKED; true`}
	if got, _ := runSandbox(t, l, probe); strings.Contains(got, "WORKED") {
		t.Fatalf("sandbox can change its %s credential copy: %s", cli, got)
	}

	spec.Env = []string{"PATH=" + filepath.Dir(bin) + ":/usr/bin:/bin", "TERM=dumb"}
	spec.Argv = append([]string{bin}, args...)

	// The run is owned by ctx: on timeout it is killed and joined before the
	// test returns, so a stalled CLI cannot outlive its subtest.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	out, runErr := runSandboxCtx(ctx, l, spec)
	if ctx.Err() != nil {
		t.Fatalf("%s did not finish within 3 minutes; run was killed and joined", cli)
	}
	if runErr != nil {
		t.Fatalf("%s exited with %v (first 400 bytes of output: %.400s)", cli, runErr, out)
	}
	var seen []string
	for _, d := range proxy.Dests() {
		seen = append(seen, d.Host+":"+d.Port)
		t.Logf("proxy log: %s:%s allowed=%v external=%v", d.Host, d.Port, d.Allowed, d.External)
	}
	if len(seen) == 0 {
		t.Fatalf("%s ran but the proxy saw no traffic", cli)
	}

	// Same sandbox, no proxy: a public address must have no route.
	direct := ly.bash(`exec 3<>/dev/tcp/1.1.1.1/443 && echo REACHED || echo refused`)
	direct.EgressSocket = spec.EgressSocket
	got, _ := runSandbox(t, l, direct)
	if !strings.Contains(got, "refused") || strings.Contains(got, "REACHED") {
		t.Errorf("direct connection from the sandbox: %s", got)
	}
}

// runSandboxCtx is runSandbox with a context that kills the run; it returns
// only after the command has exited.
func runSandboxCtx(ctx context.Context, l *Launcher, spec Spec) (string, error) {
	run, err := l.Command(ctx, spec)
	if err != nil {
		return "", err
	}
	defer run.Close()
	var out bytes.Buffer
	run.Cmd.Stdout, run.Cmd.Stderr = &out, &out
	run.Cmd.WaitDelay = 10 * time.Second
	if err := run.Start(); err != nil {
		return "", err
	}
	err = run.Cmd.Wait()
	return out.String(), err
}

// proxySpec starts proxy on a fresh Unix socket and returns a spec bridged to it.
func proxySpec(t *testing.T, ly layout, proxy *egress.Proxy) Spec {
	t.Helper()
	g := &gateHarness{t: t, ly: ly, proxy: proxy}
	g.startProxy()
	s := ly.spec("")
	s.EgressSocket = g.sock
	return s
}
