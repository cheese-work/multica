package sandbox

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/multica-ai/multica/server/internal/daemon/egress"
)

// The A7c connectivity exit gate (CHE-2216 plan section 4, A6 entry gate): a
// fake provider, the Multica server over HTTP and WebSocket, and every MCP
// server in the runtime config are reached from inside the sandbox through
// the real bridge and the real egress proxy, and nothing is reachable any
// other way. Fakes only; the real-CLI smoke is in connectivity_smoke_test.go.

// gateHarness is one run's host side: the proxy on its Unix socket, three fake
// TLS services and a CA file the sandboxed client trusts.
type gateHarness struct {
	t                         *testing.T
	l                         *Launcher
	ly                        layout
	proxy                     *egress.Proxy
	ln                        net.Listener
	sock                      string
	provider, multica, mcpURL *httptest.Server
	caFile                    string
}

// newGate starts the harness; policy options apply before the proxy serves.
func newGate(t *testing.T, policy ...func(*egress.Proxy)) *gateHarness {
	t.Helper()
	l := bridgeLauncher()
	requireSandbox(t, l)
	g := &gateHarness{t: t, l: l, ly: newLayout(t), proxy: &egress.Proxy{AllowLoopback: true}}

	for _, f := range policy {
		f(g.proxy)
	}
	g.startProxy()

	g.provider = tlsServer(t, providerMux())
	g.multica = tlsServer(t, multicaMux())
	g.mcpURL = tlsServer(t, mcpMux())

	g.caFile = filepath.Join(g.ly.work, "ca.pem")
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: g.provider.Certificate().Raw})
	mustWrite(t, g.caFile, string(ca))
	return g
}

// startProxy serves g.proxy on a fresh Unix socket.
func (g *gateHarness) startProxy() {
	g.t.Helper()
	dir, err := os.MkdirTemp("", "egress")
	if err != nil {
		g.t.Fatal(err)
	}
	g.t.Cleanup(func() { os.RemoveAll(dir) })
	g.sock = filepath.Join(dir, "egress.sock")
	if g.ln, err = net.Listen("unix", g.sock); err != nil {
		g.t.Fatal(err)
	}
	g.t.Cleanup(func() { g.ln.Close() })
	go g.proxy.Serve(g.ln)
}

func tlsServer(t *testing.T, h http.Handler) *httptest.Server {
	s := httptest.NewTLSServer(h)
	t.Cleanup(s.Close)
	return s
}

func providerMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "pong")
	})
	mux.HandleFunc("/v1/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := range 3 {
			fmt.Fprintf(w, "data: chunk-%d\n\n", i)
			w.(http.Flusher).Flush()
			time.Sleep(20 * time.Millisecond)
		}
	})
	return mux
}

func multicaMux() http.Handler {
	up := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer run-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, "pong")
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_, msg, err := c.ReadMessage()
		if err == nil {
			_ = c.WriteMessage(websocket.TextMessage, []byte("echo:"+string(msg)))
		}
	})
	return mux
}

func mcpMux() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"fake-remote-mcp"}}}`+"\n")
	})
}

func (g *gateHarness) host(s *httptest.Server) string {
	return strings.TrimPrefix(s.URL, "https://")
}

// probeSpec runs this test binary as the fake agent: probe args inside the
// sandbox, the proxy socket bridged in, the CA trusted.
func (g *gateHarness) probeSpec(args ...string) Spec {
	s := g.ly.spec("")
	s.Argv = append([]string{bridgeMount, probeArg}, args...)
	s.Env = append(s.Env, "SSL_CERT_FILE="+g.caFile)
	s.EgressSocket = g.sock
	return s
}

func (g *gateHarness) run(args ...string) (string, error) {
	g.t.Helper()
	return runSandbox(g.t, g.l, g.probeSpec(args...))
}

func (g *gateHarness) mustOK(want string, args ...string) {
	g.t.Helper()
	out, err := g.run(args...)
	if err != nil || !strings.Contains(out, want) {
		g.t.Errorf("probe %v: err=%v out=%q, want %q", args, err, out, want)
	}
}

// proxied lists the host:port pairs the proxy let through, sorted.
func (g *gateHarness) proxied() []string {
	var got []string
	for _, d := range g.proxy.Dests() {
		if d.Allowed {
			got = append(got, d.Host+":"+d.Port)
		}
	}
	sort.Strings(got)
	return got
}

func TestGateProviderReachableThroughIsolatedPath(t *testing.T) {
	g := newGate(t)
	g.mustOK("ok http 200", "http", g.provider.URL+"/v1/messages")
	g.mustOK("ok sse 3", "sse", g.provider.URL+"/v1/stream")
}

func TestGateMulticaHTTPAndWebSocketReachableThroughIsolatedPath(t *testing.T) {
	g := newGate(t)
	g.mustOK("ok http 200", "http", g.multica.URL+"/api/ping")
	g.mustOK("ok ws echo:hello", "ws", "wss://"+g.host(g.multica)+"/ws")
}

// runtimeMCPConfig is the canonical {"mcpServers": ...} document the daemon
// hands to agents (see mergeRuntimeAndAgentMcpConfig).
func (g *gateHarness) runtimeMCPConfig() string {
	exe, _ := os.Executable()
	return fmt.Sprintf(`{"mcpServers":{
	  "local-a":{"command":%q,"args":[%q]},
	  "local-b":{"command":%q,"args":[%q],"env":{"X":"1"}},
	  "remote-a":{"type":"http","url":%q},
	  "remote-b":{"type":"http","url":%q,"headers":{"Authorization":"Bearer x"}}}}`,
		exe, fakeMCPArg, exe, fakeMCPArg, g.mcpURL.URL+"/mcp", g.mcpURL.URL+"/mcp/b")
}

// gateMCPServers probes every server in cfg and returns the names it cannot
// classify, so a new transport cannot slip past the gate unprobed.
func (g *gateHarness) gateMCPServers(cfg string) (unprobed []string) {
	g.t.Helper()
	var doc struct {
		Servers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(cfg), &doc); err != nil || len(doc.Servers) == 0 {
		g.t.Fatalf("runtime MCP config: %v (%d servers)", err, len(doc.Servers))
	}
	for name, entry := range doc.Servers {
		switch {
		case entry["command"] != nil:
			args := []string{"mcp-stdio", entry["command"].(string)}
			for _, a := range entry["args"].([]any) {
				args = append(args, a.(string))
			}
			spec := g.probeSpec(args...)
			spec.ReadOnly = append(spec.ReadOnly, entry["command"].(string))
			out, err := runSandbox(g.t, g.l, spec)
			if err != nil || !strings.Contains(out, "ok mcp-stdio fake-stdio-mcp") {
				g.t.Errorf("MCP server %s: err=%v out=%q", name, err, out)
			}
		case entry["url"] != nil:
			g.mustOK("ok mcp-http fake-remote-mcp", "mcp-http", entry["url"].(string))
		default:
			unprobed = append(unprobed, name)
		}
	}
	return unprobed
}

func TestGateEachRuntimeMCPServerReachableThroughIsolatedPath(t *testing.T) {
	g := newGate(t)
	if unprobed := g.gateMCPServers(g.runtimeMCPConfig()); len(unprobed) != 0 {
		t.Errorf("MCP servers the gate could not probe: %v", unprobed)
	}
	var remote int
	for _, d := range g.proxied() {
		if d == g.host(g.mcpURL) {
			remote++
		}
	}
	if remote != 2 {
		t.Errorf("proxy saw %d connections to the remote MCP server, want 2: %v", remote, g.proxied())
	}
}

func TestGateReportsMCPServerItCannotProbe(t *testing.T) {
	g := newGate(t)
	got := g.gateMCPServers(`{"mcpServers":{"odd":{"type":"carrier-pigeon"}}}`)
	if len(got) != 1 || got[0] != "odd" {
		t.Errorf("unprobed = %v, want [odd]", got)
	}
}

// TestGateProxyLogShowsEveryDestination is the A6 evidence that the traffic
// went through the proxy: every service the run used is in the destination log.
func TestGateProxyLogShowsEveryDestination(t *testing.T) {
	g := newGate(t)
	g.mustOK("ok http", "http", g.provider.URL+"/v1/messages")
	g.mustOK("ok http", "http", g.multica.URL+"/api/ping")
	g.mustOK("ok ws", "ws", "wss://"+g.host(g.multica)+"/ws")
	g.mustOK("ok mcp-http", "mcp-http", g.mcpURL.URL+"/mcp")
	got := strings.Join(g.proxied(), " ")
	for _, s := range []*httptest.Server{g.provider, g.multica, g.mcpURL} {
		if !strings.Contains(got, g.host(s)) {
			t.Errorf("proxy log lacks %s: %s", g.host(s), got)
		}
	}
}

// No fallback: whatever the agent does, the only way out is the proxy.

func TestGateNoDirectRouteToAnyService(t *testing.T) {
	g := newGate(t)
	for _, s := range []*httptest.Server{g.provider, g.multica, g.mcpURL} {
		g.mustOK("refused", "direct", g.host(s))
		g.mustOK("refused", "noproxy-http", s.URL)
	}
	if n := len(g.proxy.Dests()); n != 0 {
		t.Errorf("direct attempts reached the proxy log: %v", g.proxy.Dests())
	}
}

func TestGateProxyDeathMidRunLeavesNoRoute(t *testing.T) {
	g := newGate(t)
	spec := g.probeSpec()
	probe := strings.Join([]string{bridgeMount, probeArg}, " ")
	url := g.provider.URL + "/v1/messages"
	spec.Argv = []string{"/bin/sh", "-c", fmt.Sprintf(`
		%[1]s http %[2]s || exit 2
		touch %[3]s/first-done
		i=0; while [ ! -e %[3]s/go ] && [ $i -lt 100 ]; do sleep 0.1; i=$((i+1)); done
		%[1]s http %[2]s && exit 3
		%[1]s direct %[4]s || exit 4
		echo STILL-ISOLATED`, probe, url, g.ly.work, g.host(g.provider))}
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := runSandbox(t, g.l, spec)
		done <- result{out, err}
	}()
	waitForFile(t, filepath.Join(g.ly.work, "first-done"))
	g.ln.Close() // the proxy goes away, as if the daemon lost it
	mustWrite(t, filepath.Join(g.ly.work, "go"), "")
	select {
	case r := <-done:
		if r.err != nil || !strings.Contains(r.out, "STILL-ISOLATED") {
			t.Errorf("after proxy death: err=%v out=%q", r.err, r.out)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run did not finish")
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	for range 100 {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

func TestGateBridgeBinaryMissingStopsRunBeforeAgent(t *testing.T) {
	g := newGate(t)
	g.l.BridgeExe = filepath.Join(g.ly.root, "no-such-bridge")
	spec := g.ly.spec("touch " + filepath.Join(g.ly.work, "agent-ran"))
	spec.EgressSocket = g.sock
	if _, err := g.l.Command(context.Background(), spec); err == nil {
		t.Error("run was prepared without a bridge binary")
	}
	if _, err := os.Stat(filepath.Join(g.ly.work, "agent-ran")); err == nil {
		t.Error("agent ran without a bridge")
	}
}

func TestGatePolicyRefusalReachesTheClient(t *testing.T) {
	g := newGate(t, func(p *egress.Proxy) { p.Allow = func(_, _ string) bool { return false } })
	out, err := g.run("http", g.provider.URL+"/v1/messages")
	if err == nil || !strings.Contains(out, "Forbidden") {
		t.Errorf("refused destination: err=%v out=%q, want the proxy refusal (Forbidden)", err, out)
	}
}
