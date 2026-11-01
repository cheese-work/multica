//go:build agentintegration && linux

package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	baseURL, token := os.Getenv("ANTHROPIC_BASE_URL"), os.Getenv("ANTHROPIC_AUTH_TOKEN")
	host, port, err := smokeEndpoint(baseURL)
	if err != nil || token == "" {
		t.Fatalf("smoke needs ANTHROPIC_BASE_URL (https) and ANTHROPIC_AUTH_TOKEN in the environment (err=%v, token set=%v)", err, token != "")
	}
	l := bridgeLauncher()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := l.SelfCheck(ctx); err != nil {
		t.Fatalf("smoke was authorized but the host cannot run the sandbox: %v", err)
	}
	result, err := runSmokeProviders(t.Run, smokeProviders(baseURL, token), func(t *testing.T, p smokeProvider) {
		smokeCLI(t, l, p, host, port, token)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("SMOKE RESULT claude=%s codex=%s", result["claude"], result["codex"])
}

type smokeProvider struct {
	cli  string
	args []string
	env  []string
}

// smokeProviders builds both providers for one gateway. The token goes into
// each CLI's environment only, never into argv.
func smokeProviders(baseURL, token string) []smokeProvider {
	const prompt = "Reply with the single word OK."
	model := os.Getenv("MULTICA_SMOKE_CODEX_MODEL")
	if model == "" {
		model = "gpt-6-luna" // gpt-6-astra and gpt-5.6-terra need a composite route on this gateway
	}
	provider := fmt.Sprintf(`model_providers.smoke={name="smoke",base_url=%q,wire_api="responses",env_key="OPENAI_API_KEY"}`, baseURL)
	return []smokeProvider{
		{"claude", []string{"-p", prompt},
			[]string{"ANTHROPIC_BASE_URL=" + baseURL, "ANTHROPIC_AUTH_TOKEN=" + token, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"}},
		{"codex", []string{"exec", "--skip-git-repo-check", "-c", "model_provider=smoke", "-c", provider, "-m", model, prompt},
			[]string{"OPENAI_API_KEY=" + token}},
	}
}

// smokeEndpoint splits an https gateway URL into the one host and port the
// proxy may reach.
func smokeEndpoint(baseURL string) (host, port string, err error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return "", "", fmt.Errorf("gateway base URL must be https://host[:port]/...")
	}
	port = u.Port()
	if port == "" {
		port = "443"
	}
	return u.Hostname(), port, nil
}

// allowOnly is the proxy policy for the smoke: exactly one host:port, no wildcard.
func allowOnly(host, port string) func(host, port string) bool {
	return func(h, p string) bool { return strings.EqualFold(h, host) && p == port }
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// scrub keeps the token out of failure output.
func scrub(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "<token>")
}

// runSmokeProviders runs one subtest per provider. A provider is PASS only if
// its subtest did not fail (a non-fatal t.Errorf counts); the run is accepted
// only when every provider is PASS. run is t.Run in the real test.
func runSmokeProviders(run func(string, func(*testing.T)) bool, providers []smokeProvider, one func(*testing.T, smokeProvider)) (map[string]string, error) {
	result := map[string]string{}
	for _, p := range providers {
		if run(p.cli, func(t *testing.T) { one(t, p) }) {
			result[p.cli] = "PASS"
		} else {
			result[p.cli] = "FAIL"
		}
	}
	for _, p := range providers {
		if result[p.cli] != "PASS" {
			return result, fmt.Errorf("smoke needs every provider to pass: %v", result)
		}
	}
	return result, nil
}

// A provider whose subtest failed after smokeCLI recorded its problem with a
// non-fatal error must never be labelled PASS. Deterministic: no CLI runs.
func TestSmokeFailedProviderIsNeverPass(t *testing.T) {
	failing := func(name string, f func(*testing.T)) bool { return name != "codex" }
	result, err := runSmokeProviders(failing, smokeProviders("https://gw.example/v1", "x"), func(*testing.T, smokeProvider) {})
	if result["codex"] != "FAIL" || result["claude"] != "PASS" {
		t.Fatalf("result = %v, want claude=PASS codex=FAIL", result)
	}
	if err == nil {
		t.Fatal("a run with a failed provider was accepted")
	}
	allOK := func(name string, f func(*testing.T)) bool { return true }
	if _, err := runSmokeProviders(allOK, smokeProviders("https://gw.example/v1", "x"), func(*testing.T, smokeProvider) {}); err != nil {
		t.Fatalf("all providers passed but run rejected: %v", err)
	}
}

func smokeCLI(t *testing.T, l *Launcher, p smokeProvider, host, port, token string) {
	bin, err := exec.LookPath(p.cli)
	if err != nil {
		t.Fatalf("%s is not installed: %v", p.cli, err)
	}
	if real, err := filepath.EvalSymlinks(bin); err == nil {
		bin = real
	}
	ly := newLayout(t)

	// Only the gateway host:port is allowed through the proxy.
	proxy := &egress.Proxy{Trusted: []string{host}, Allow: allowOnly(host, port)}
	spec := proxySpec(t, ly, proxy)
	// The provider credential is the token in the environment. A placeholder
	// credential file keeps the read-only bind and its mutation probe in the
	// smoke; it holds no secret.
	credential := ".smoke/credential"
	spec.ReadOnly = append(spec.ReadOnly, putCredential(t, ly, credential, "placeholder"), filepath.Dir(bin))
	for _, ro := range strings.Split(os.Getenv("MULTICA_SMOKE_READONLY"), ":") {
		if ro != "" {
			spec.ReadOnly = append(spec.ReadOnly, ro)
		}
	}

	// The run must not be able to change the copy (mode bits are not enough).
	probe := spec
	probe.Argv = []string{"/bin/sh", "-c", `chmod 600 "$HOME/` + credential + `" 2>/dev/null && echo CHMOD-WORKED; echo x >> "$HOME/` + credential + `" 2>/dev/null && echo APPEND-WORKED; true`}
	if got, _ := runSandbox(t, l, probe); strings.Contains(got, "WORKED") {
		t.Fatalf("sandbox can change its %s credential copy: %s", p.cli, got)
	}

	spec.Env = append([]string{"PATH=" + filepath.Dir(bin) + ":/usr/bin:/bin", "TERM=dumb"}, p.env...)
	spec.Argv = append([]string{bin}, p.args...)

	// The run is owned by ctx: on timeout it is killed and joined before the
	// test returns, so a stalled CLI cannot outlive its subtest.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	out, runErr := runSandboxCtx(ctx, l, spec)
	if ctx.Err() != nil {
		t.Fatalf("%s did not finish within 3 minutes; run was killed and joined", p.cli)
	}
	if runErr != nil {
		t.Fatalf("%s exited with %v (last 600 bytes of output: %s)", p.cli, runErr, tail(scrub(out, token), 600))
	}
	reached := false
	for _, d := range proxy.Dests() {
		t.Logf("proxy log: %s:%s allowed=%v external=%v", d.Host, d.Port, d.Allowed, d.External)
		reached = reached || (d.Allowed && strings.EqualFold(d.Host, host) && d.Port == port)
	}
	if !reached {
		t.Fatalf("%s ran but the proxy saw no allowed traffic to the gateway", p.cli)
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

func TestSmokeEndpointAllowsOnlyTheExactHostAndPort(t *testing.T) {
	host, port, err := smokeEndpoint("https://gw.example.ts.net:31344/v1")
	if err != nil || host != "gw.example.ts.net" || port != "31344" {
		t.Fatalf("got %q %q %v", host, port, err)
	}
	if _, port, _ := smokeEndpoint("https://gw.example.ts.net/v1"); port != "443" {
		t.Fatalf("default port = %q, want 443", port)
	}
	for _, bad := range []string{"", "http://gw.example.ts.net:31344/v1", "https:///v1", "gw.example.ts.net:31344"} {
		if _, _, err := smokeEndpoint(bad); err == nil {
			t.Errorf("endpoint %q accepted", bad)
		}
	}
	allow := allowOnly(host, port)
	for _, c := range []struct {
		host, port string
		want       bool
	}{
		{"gw.example.ts.net", "31344", true},
		{"GW.example.ts.net", "31344", true},
		{"gw.example.ts.net", "443", false},
		{"1.1.1.1", "31344", false},
		{"api.anthropic.com", "443", false},
		{"evil.gw.example.ts.net", "31344", false},
	} {
		if got := allow(c.host, c.port); got != c.want {
			t.Errorf("allow(%s:%s) = %v, want %v", c.host, c.port, got, c.want)
		}
	}
}

// The token reaches each CLI by environment variable only: never in argv
// (visible in ps and in failure output) and never in the provider list.
func TestSmokeProvidersCarryTheTokenInEnvOnly(t *testing.T) {
	const url, token = "https://gw.example.ts.net:31344/v1", "tok-not-a-secret-123"
	ps := smokeProviders(url, token)
	if len(ps) != 2 || ps[0].cli != "claude" || ps[1].cli != "codex" {
		t.Fatalf("providers = %+v", ps)
	}
	wantVar := map[string]string{"claude": "ANTHROPIC_AUTH_TOKEN", "codex": "OPENAI_API_KEY"}
	for _, p := range ps {
		for _, a := range p.args {
			if strings.Contains(a, token) {
				t.Errorf("%s: token in argv", p.cli)
			}
		}
		if !slices.Contains(p.env, wantVar[p.cli]+"="+token) {
			t.Errorf("%s: %s not set in env", p.cli, wantVar[p.cli])
		}
		if !strings.Contains(strings.Join(append(p.args, p.env...), " "), url) {
			t.Errorf("%s: base URL not passed", p.cli)
		}
	}
	if got := scrub("auth failed for "+token+" at gw", token); strings.Contains(got, token) {
		t.Errorf("scrub left the token: %s", got)
	}
}
