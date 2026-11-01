//go:build linux

package daemon

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/egress"
	"github.com/multica-ai/multica/server/internal/daemon/sandbox"
	"github.com/multica-ai/multica/server/internal/daemon/sandbox/gateprobe"
)

// The MCP part of the A7c connectivity exit gate (CHE-2216): every server of
// the config the daemon really builds for a run is reached from inside the
// sandbox through the bridge and the egress proxy. It lives here because the
// merge is private to this package; the sandbox package holds the rest of the
// gate. Fakes only: the stdio server needs its `env` value, the remote one its
// `headers`, so an entry whose fields are dropped fails.

const (
	gateRemoteHeader = "X-Gate-Key"
	gateRemoteKey    = "remote-key-1"
	gateStdioKey     = "stdio-key-1"
)

// gateRemoteMCP answers initialize on /with-headers only when the header is
// there and on /plain without one.
func gateRemoteMCP() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/with-headers" && r.Header.Get(gateRemoteHeader) != gateRemoteKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.Copy(io.Discard, r.Body)
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"fake-remote-mcp:%s"}}}`+"\n", r.URL.Path)
	})
}

type mcpGate struct {
	t      *testing.T
	l      *sandbox.Launcher
	proxy  *egress.Proxy
	sock   string
	remote *httptest.Server
	caFile string
	work   string
	tmp    string
	home   string
}

func newMCPGate(t *testing.T) *mcpGate {
	t.Helper()
	l := &sandbox.Launcher{Mode: sandbox.ModeOn, BridgeExe: os.Args[0], Deny: sandbox.DefaultDeny(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := l.SelfCheck(ctx); err != nil {
		t.Skipf("host cannot run the sandbox: %v", err)
	}
	g := &mcpGate{t: t, l: l, proxy: &egress.Proxy{AllowLoopback: true}}
	root := t.TempDir()
	g.work, g.tmp, g.home = filepath.Join(root, "work"), filepath.Join(root, "tmp"), filepath.Join(root, "home")
	for _, d := range []string{g.work, g.tmp, g.home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	sockDir, err := os.MkdirTemp("", "egress")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	g.sock = filepath.Join(sockDir, "egress.sock")
	ln, err := net.Listen("unix", g.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go g.proxy.Serve(ln)

	g.remote = httptest.NewTLSServer(gateRemoteMCP())
	t.Cleanup(g.remote.Close)
	g.caFile = filepath.Join(g.work, "ca.pem")
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: g.remote.Certificate().Raw})
	if err := os.WriteFile(g.caFile, ca, 0o644); err != nil {
		t.Fatal(err)
	}
	return g
}

// probe runs the entry as an agent CLI would start or call it, inside the sandbox.
func (g *mcpGate) probe(entry any) (string, error) {
	g.t.Helper()
	raw, err := json.Marshal(entry)
	if err != nil {
		g.t.Fatal(err)
	}
	exe, _ := os.Executable()
	run, err := g.l.Command(context.Background(), sandbox.Spec{
		WorkDir: g.work, TempDir: g.tmp, Home: g.home,
		ReadOnly:     []string{exe},
		Env:          []string{"PATH=/usr/bin:/bin", "SSL_CERT_FILE=" + g.caFile},
		Argv:         []string{"/run/multica-bridge", gateprobe.ProbeArg, "mcp", string(raw)},
		EgressSocket: g.sock,
	})
	if err != nil {
		return "", err
	}
	defer run.Close()
	var out strings.Builder
	run.Cmd.Stdout, run.Cmd.Stderr = &out, &out
	if err := run.Start(); err != nil {
		return "", err
	}
	err = run.Cmd.Wait()
	return out.String(), err
}

func TestGateEveryMergedRuntimeMCPServerReachableThroughIsolatedPath(t *testing.T) {
	g := newMCPGate(t)
	exe, _ := os.Executable()

	// The runtime layer (the user's ~/.claude.json) and the agent layer, merged
	// by the daemon's own code; the agent wins on "shared".
	home := t.TempDir()
	redirectTestHome(t, home)
	runtimeDoc := map[string]any{"mcpServers": map[string]any{
		"runtime-stdio": map[string]any{"command": exe, "args": []string{gateprobe.FakeMCPArg}, "env": map[string]string{gateprobe.GateKeyEnv: gateStdioKey}},
		"runtime-http":  map[string]any{"type": "http", "url": g.remote.URL + "/with-headers", "headers": map[string]string{gateRemoteHeader: gateRemoteKey}},
		"shared":        map[string]any{"command": "runtime-copy-must-lose"},
	}}
	agentDoc := map[string]any{"mcpServers": map[string]any{
		"agent-http": map[string]any{"type": "http", "url": g.remote.URL + "/plain"},
		"shared":     map[string]any{"command": exe, "args": []string{gateprobe.FakeMCPArg}, "env": map[string]string{gateprobe.GateKeyEnv: "shared-key"}},
	}}
	mustJSONFile(t, filepath.Join(home, ".claude.json"), runtimeDoc)
	agentRaw, _ := json.Marshal(agentDoc)

	merged, err := mergeRuntimeAndAgentMcpConfig("claude", agentRaw)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Servers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(merged, &doc); err != nil {
		t.Fatal(err)
	}
	want := []string{"agent-http", "runtime-http", "runtime-stdio", "shared"}
	var names []string
	for name := range doc.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("merged config has servers %v, want %v", names, want)
	}

	expect := map[string]string{
		"runtime-stdio": "ok mcp-stdio fake-stdio-mcp:" + gateStdioKey,
		"shared":        "ok mcp-stdio fake-stdio-mcp:shared-key",
		"runtime-http":  "ok mcp-http fake-remote-mcp:/with-headers",
		"agent-http":    "ok mcp-http fake-remote-mcp:/plain",
	}
	var tested []string
	for _, name := range names {
		var entry gateprobe.MCPEntry
		if err := json.Unmarshal(doc.Servers[name], &entry); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out, err := g.probe(entry)
		if err != nil || !strings.Contains(out, expect[name]) {
			t.Errorf("MCP server %s: err=%v out=%q, want %q", name, err, out, expect[name])
		}
		tested = append(tested, name)
	}
	t.Logf("MCP servers tested through the isolated path: %v", tested)
	if len(tested) != len(doc.Servers) {
		t.Errorf("tested %v of %d configured servers", tested, len(doc.Servers))
	}

	var viaProxy int
	for _, d := range g.proxy.Dests() {
		if d.Allowed && d.Host+":"+d.Port == strings.TrimPrefix(g.remote.URL, "https://") {
			viaProxy++
		}
	}
	if viaProxy != 2 {
		t.Errorf("proxy saw %d connections to the remote MCP server, want 2: %v", viaProxy, g.proxy.Dests())
	}
}

// A remote server that wants its headers refuses an entry without them, so the
// gate above fails when a header is dropped.
func TestGateRemoteMCPServerNeedsItsHeaders(t *testing.T) {
	g := newMCPGate(t)
	out, err := g.probe(gateprobe.MCPEntry{URL: g.remote.URL + "/with-headers"})
	if err == nil || !strings.Contains(out, "status 401") {
		t.Errorf("headerless call: err=%v out=%q, want status 401", err, out)
	}
}

// A stdio server that wants its env fails without it.
func TestGateStdioMCPServerNeedsItsEnv(t *testing.T) {
	g := newMCPGate(t)
	exe, _ := os.Executable()
	out, err := g.probe(gateprobe.MCPEntry{Command: exe, Args: []string{gateprobe.FakeMCPArg}})
	if err == nil || !strings.Contains(out, gateprobe.GateKeyEnv) {
		t.Errorf("envless start: err=%v out=%q, want a missing-%s error", err, out, gateprobe.GateKeyEnv)
	}
}

func mustJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}
