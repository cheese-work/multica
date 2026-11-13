package sandbox

import (
	"context"
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
	"github.com/multica-ai/multica/server/internal/daemon/sandbox/gateprobe"
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
	return newGateWith(t, func(g *gateHarness) {
		for _, f := range policy {
			f(g.proxy)
		}
	})
}

// newGateWith is newGate with a hook that sees the fake services (so a policy
// can name their ports) after they exist and before the proxy serves.
func newGateWith(t *testing.T, before func(*gateHarness)) *gateHarness {
	t.Helper()
	l := bridgeLauncher()
	requireSandbox(t, l)
	g := &gateHarness{t: t, l: l, ly: newLayout(t), proxy: &egress.Proxy{AllowLoopback: true}}

	g.provider = tlsServer(t, providerMux())
	g.multica = tlsServer(t, multicaMux())
	g.mcpURL = tlsServer(t, mcpMux())
	before(g)
	g.startProxy()

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
	ack := make(chan struct{}, 1)
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "pong")
	})
	// Events 1 and 2 wait for the client's ack of event 0 (see gateprobe), so
	// the stream only completes when events are delivered as they are sent.
	mux.HandleFunc("/v1/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: chunk-0\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-ack:
		case <-time.After(10 * time.Second):
			return
		}
		for i := 1; i < 3; i++ {
			fmt.Fprintf(w, "data: chunk-%d\n\n", i)
			w.(http.Flusher).Flush()
		}
	})
	mux.HandleFunc(gateprobe.SSEAckPath, func(w http.ResponseWriter, r *http.Request) {
		ack <- struct{}{}
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
	s.Argv = append([]string{bridgeMount, gateprobe.ProbeArg}, args...)
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

// TestGateProxyLogShowsEveryDestination is the A6 evidence that the traffic
// went through the proxy: every service the run used is in the destination log.
func TestGateProxyLogShowsEveryDestination(t *testing.T) {
	g := newGate(t)
	g.mustOK("ok http", "http", g.provider.URL+"/v1/messages")
	g.mustOK("ok http", "http", g.multica.URL+"/api/ping")
	g.mustOK("ok ws", "ws", "wss://"+g.host(g.multica)+"/ws")
	g.mustOK("ok mcp-http", "mcp", `{"url":"`+g.mcpURL.URL+`/mcp"}`)
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
	probe := strings.Join([]string{bridgeMount, gateprobe.ProbeArg}, " ")
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

// putCredential writes a 0400 copy of a provider credential at rel under the
// run HOME and returns its path. Mode bits do not stop the run's own user from
// chmod, replace or delete inside its writable HOME, so callers also bind the
// returned path read-only over itself (Spec.ReadOnly).
func putCredential(t *testing.T, ly layout, rel, body string) string {
	t.Helper()
	path := filepath.Join(ly.home, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o400); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGateCredentialCopyCannotBeChangedInsideSandbox(t *testing.T) {
	l := bridgeLauncher()
	requireSandbox(t, l)
	ly := newLayout(t)
	const rel, body = ".claude/.credentials.json", "fake-credential-not-a-secret"
	// Each attempt must fail; the file is read back last.
	script := `f="$HOME/` + rel + `"
		chmod 600 "$f" 2>/dev/null && echo CHMOD-WORKED
		echo changed >> "$f" 2>/dev/null && echo APPEND-WORKED
		mv "$f" "$f.moved" 2>/dev/null && echo RENAME-WORKED
		rm -f "$f" 2>/dev/null; [ -e "$f" ] || echo DELETE-WORKED
		printf x > "$f.new" && mv -f "$f.new" "$f" 2>/dev/null && echo REPLACE-WORKED
		cat "$f"`
	spec := ly.spec(script)
	spec.ReadOnly = append(spec.ReadOnly, putCredential(t, ly, rel, body))
	out, err := runSandbox(t, l, spec)
	if err != nil && !strings.Contains(out, body) {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if strings.Contains(out, "WORKED") {
		t.Errorf("sandbox changed its credential copy:\n%s", out)
	}
	if !strings.Contains(out, body) {
		t.Errorf("credential copy no longer readable as written:\n%s", out)
	}
	if got, _ := os.ReadFile(filepath.Join(ly.home, rel)); string(got) != body {
		t.Errorf("host copy changed to %q", got)
	}
}
