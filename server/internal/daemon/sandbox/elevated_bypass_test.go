package sandbox

import (
	"context"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/egress"
)

// T17 (a) to (f) of CHE-2216 plan section 6, slice B1b: an elevated run, in the
// real sandbox and behind the real elevated egress proxy, cannot reach GitHub,
// a GitHub IP literal, a relay or CDN name, an allowed host under a foreign
// SNI, or anything without the proxy. Fakes only: a fake resolver and a local
// TLS server stand in for the network. T17 (g) to (i) belong to other slices.

const (
	bypassMulticaHost  = "multica.bypass.test"
	bypassProviderHost = "api.provider.test"
	// github.com's address, spelled as the plan's literal forms.
	ghV4, ghV6, ghMapped, ghInt, ghHex, ghOctal = "140.82.112.3", "2606:50c0:8000::154", "::ffff:140.82.112.3", "2354212867", "0x8c527003", "0214.0122.0160.03"
)

type bypassFixture struct {
	*gateHarness
	port string
}

// resolverFunc is a fake name service: the Multica test host is the local TLS
// server; everything else is a public address the proxy would dial if it let
// the name through.
type resolverFunc func(host string) []netip.Addr

func (f resolverFunc) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	return f(host), nil
}

func newBypassGate(t *testing.T) *bypassFixture {
	t.Helper()
	var port string
	g := newGateWith(t, func(g *gateHarness) {
		u, _ := url.Parse(g.multica.URL)
		port = u.Port()
		lo := netip.MustParseAddr("127.0.0.1")
		res := resolverFunc(func(h string) []netip.Addr {
			if h == bypassMulticaHost {
				return []netip.Addr{lo}
			}
			return []netip.Addr{netip.MustParseAddr("203.0.113.9")}
		})
		pol, err := egress.NewElevatedPolicy(bypassMulticaHost, []string{port}, []string{bypassProviderHost}, []netip.Addr{lo}, res)
		if err != nil {
			t.Fatal(err)
		}
		g.proxy.Elevated = pol
		g.proxy.Trusted = []string{bypassMulticaHost, bypassProviderHost}
	})
	return &bypassFixture{gateHarness: g, port: port}
}

// dests is the proxy log without the bridge's own start-up connection, which
// reaches the proxy and closes without sending a request.
func (f *bypassFixture) dests() []egress.Dest {
	var out []egress.Dest
	for _, d := range f.proxy.Dests() {
		if d.Host != "" || d.Reason != "unparsable request" {
			out = append(out, d)
		}
	}
	return out
}

// connect runs the probe's raw CONNECT and returns its one-line result.
func (f *bypassFixture) connect(authority string, sni ...string) string {
	f.t.Helper()
	args := append([]string{"connect", authority}, sni...)
	out, err := f.run(args...)
	if err != nil {
		f.t.Fatalf("connect %s: probe error: %v\n%s", authority, err, out)
	}
	return strings.TrimSpace(out)
}

// wantNoTunnel asserts the client saw a refusal and the proxy allowed nothing.
func (f *bypassFixture) wantNoTunnel(authority, out string) {
	f.t.Helper()
	if !strings.HasPrefix(out, "connect "+authority+" status=") || strings.Contains(out, "status=200") {
		f.t.Errorf("CONNECT %s: got %q, want a refusal", authority, out)
	}
	f.wantNothingAllowed()
}

func (f *bypassFixture) wantNothingAllowed() {
	f.t.Helper()
	for _, d := range f.dests() {
		if d.Allowed {
			f.t.Errorf("proxy allowed %+v", d)
		}
	}
}

// Control: the same path does carry an allowed host with a matching SNI, so the
// refusals below are not an artefact of a broken harness.
func TestBypassControlAllowedHostTunnelsWithMatchingSNI(t *testing.T) {
	f := newBypassGate(t)
	a := net.JoinHostPort(bypassMulticaHost, f.port)
	if out := f.connect(a, bypassMulticaHost); out != "connect "+a+" status=200 tls=ok" {
		t.Errorf("control CONNECT: %q", out)
	}
}

// (a) the tools an agent would use: curl and git against GitHub.
func TestBypassCurlAndGitToGitHubFail(t *testing.T) {
	f := newBypassGate(t)
	for name, script := range map[string]string{
		"curl api.github.com":        "curl -sS --max-time 10 https://api.github.com/ && echo REACHED",
		"git fetch github.com":       "git ls-remote https://github.com/cheese-work/multica.git && echo REACHED",
		"curl raw.githubusercontent": "curl -sS --max-time 10 https://raw.githubusercontent.com/cheese-work/multica/main/README.md && echo REACHED",
	} {
		spec := f.ly.spec(script)
		spec.EgressSocket = f.sock
		out, _ := runSandbox(t, f.l, spec)
		if strings.Contains(out, "REACHED") {
			t.Errorf("%s reached GitHub:\n%s", name, out)
		}
		if !strings.Contains(out, "403") && !strings.Contains(out, "CONNECT") {
			t.Errorf("%s: no proxy refusal in output:\n%s", name, out)
		}
	}
	assertNothingAllowed(t, f)
}

func assertNothingAllowed(t *testing.T, f *bypassFixture) {
	t.Helper()
	if len(f.dests()) == 0 {
		t.Error("the proxy saw nothing, so the attempt did not go through it")
	}
	f.wantNothingAllowed()
	if f.proxy.ExternalRead() {
		t.Error("run was marked external_read by a refused destination")
	}
}

// (b) CONNECT to GitHub IP literals in every spelling.
func TestBypassConnectToIPLiteralsRefused(t *testing.T) {
	for name, a := range map[string]string{
		"ipv4":        ghV4 + ":443",
		"ipv6":        "[" + ghV6 + "]:443",
		"ipv4-mapped": "[" + ghMapped + "]:443",
		"integer":     ghInt + ":443",
		"hex":         ghHex + ":443",
		"octal":       ghOctal + ":443",
		"user-info":   "user@" + bypassMulticaHost + ":443",
		"loopback":    "127.0.0.1:443",
		"metadata-ip": "169.254.169.254:443",
	} {
		t.Run(name, func(t *testing.T) {
			f := newBypassGate(t)
			f.wantNoTunnel(a, f.connect(a))
		})
	}
}

// (c) alternate, relay and CDN names, and the case and trailing-dot variants
// of the one GitHub host.
func TestBypassConnectToRelayAndAlternateNamesRefused(t *testing.T) {
	for _, h := range []string{
		"API.GITHUB.COM.", "api.github.com", "github.com", "raw.githubusercontent.com", "objects.githubusercontent.com",
		"codeload.github.com", "github.global.ssl.fastly.net", "cdn.jsdelivr.net", "proxy.golang.org",
	} {
		t.Run(h, func(t *testing.T) {
			f := newBypassGate(t)
			a := h + ":443"
			f.wantNoTunnel(a, f.connect(a))
		})
	}
}

// (d) the Multica host carrying a GitHub SNI: the tunnel is cut at the hello
// and nothing is logged as allowed. The provider host cannot be exercised here:
// it is reachable on 443 only, which the fixture cannot serve, and the proxy
// dials before it reads the hello. That case (provider host, foreign SNI) is
// egress.TestElevatedSNIMismatchRefused, which injects the dialer.
func TestBypassAllowedHostWithForeignSNIRefused(t *testing.T) {
	f := newBypassGate(t)
	a := net.JoinHostPort(bypassMulticaHost, f.port)
	for _, sni := range []string{"api.github.com", "GITHUB.COM", "raw.githubusercontent.com"} {
		if out := f.connect(a, sni); out != "connect "+a+" status=200 tls=fail" {
			t.Errorf("SNI %s: %q, want the tunnel to be cut at the hello", sni, out)
		}
	}
	if n := len(f.dests()); n != 3 {
		t.Errorf("proxy logged %d attempts, want 3", n)
	}
	for _, d := range f.dests() {
		if d.Allowed {
			t.Errorf("foreign SNI left an allowed entry: %+v", d)
		}
		if !strings.Contains(d.Reason, "SNI") {
			t.Errorf("refusal reason %q does not name the SNI", d.Reason)
		}
	}
}

// (e) absolute-form HTTP request to the proxy instead of CONNECT.
func TestBypassAbsoluteFormRequestRefused(t *testing.T) {
	f := newBypassGate(t)
	for _, u := range []string{"http://github.com/", "http://api.github.com/user", "http://" + ghV4 + "/"} {
		if out, want := strings.TrimSpace(mustRun(t, f, "absolute", u)), "absolute "+u+" status=405"; out != want {
			t.Errorf("absolute-form %s: %q, want %q", u, out, want)
		}
	}
	f.wantNothingAllowed()
}

func mustRun(t *testing.T, f *bypassFixture, args ...string) string {
	t.Helper()
	out, err := f.run(args...)
	if err != nil {
		t.Fatalf("probe %v: %v\n%s", args, err, out)
	}
	return out
}

// (f) the proxy is the only way out: no direct TCP, no DNS, no proxy-less HTTP.
func TestBypassNoDirectTCPAndNoDNSWithoutTheProxy(t *testing.T) {
	f := newBypassGate(t)
	for _, target := range []string{net.JoinHostPort(ghV4, "443"), net.JoinHostPort(ghV6, "443"), "1.1.1.1:443", "8.8.8.8:53"} {
		if out := mustRun(t, f, "direct", target); !strings.Contains(out, "refused") {
			t.Errorf("direct %s: %q", target, out)
		}
	}
	if out := mustRun(t, f, "noproxy-http", "https://api.github.com/"); !strings.Contains(out, "refused") {
		t.Errorf("proxy-less https: %q", out)
	}
	for _, name := range []string{"api.github.com", "github.com", bypassMulticaHost} {
		if out := mustRun(t, f, "dns", name); !strings.Contains(out, "refused") {
			t.Errorf("dns %s: %q, want no answer", name, out)
		}
	}
	if n := len(f.dests()); n != 0 {
		t.Errorf("proxy-less attempts reached the proxy log: %v", f.dests())
	}
}
