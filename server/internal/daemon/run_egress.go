package daemon

import (
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	"github.com/multica-ai/multica/server/internal/daemon/egress"
	"github.com/multica-ai/multica/server/internal/daemon/sandbox"
)

// providerAPIHosts are the model API hosts a provider's CLI talks to. They are
// trusted: contacting them never marks the run external_read (plan 3.7).
var providerAPIHosts = map[string][]string{
	"claude": {"api.anthropic.com"},
	"codex":  {"api.openai.com", "chatgpt.com"},
}

// egressTrustedHosts is the run's Trusted list: the Multica server host plus
// the provider's API hosts. An unparsable server URL adds nothing, so the
// server host then counts as external, which is the safe direction.
func egressTrustedHosts(serverURL, provider string) []string {
	var hosts []string
	if u, err := url.Parse(serverURL); err == nil && u.Hostname() != "" {
		hosts = append(hosts, u.Hostname())
	}
	return append(hosts, providerAPIHosts[provider]...)
}

// runEgressAllowLoopback lets the proxy dial loopback, so a test can reach a
// local origin. A var only for that; production never sets it.
var runEgressAllowLoopback = false

// runEgress is one run's egress proxy (CHE-6502, A4d-4): the run's only
// network path under enforcement on. The listener is a Unix socket in a
// per-run 0700 directory; the sandbox bridge forwards to it. The policy is the
// restricted one (every hostname) until CHE-2224 sets an allowlist.
type runEgress struct {
	Socket string // host path; pass as sandbox.Spec.EgressSocket

	proxy  *egress.Proxy
	ln     net.Listener
	dir    string
	log    *slog.Logger
	done   chan struct{}
	mu     sync.Mutex
	closed bool
}

// startRunEgress starts the run's proxy. Only enforcement on starts one: off
// and audit return nil, nil and leave no socket. The caller closes it when the
// run ends.
func startRunEgress(mode sandbox.Mode, trusted []string, log *slog.Logger) (*runEgress, error) {
	if mode != sandbox.ModeOn {
		return nil, nil
	}
	// Short path: a Unix socket path is limited to about 100 bytes. MkdirTemp
	// creates the directory with mode 0700.
	dir, err := os.MkdirTemp("", "mre-")
	if err != nil {
		return nil, err
	}
	sock := filepath.Join(dir, "e.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	re := &runEgress{Socket: sock, ln: ln, dir: dir, log: log, done: make(chan struct{})}
	re.proxy = &egress.Proxy{
		Trusted:       trusted,
		AllowLoopback: runEgressAllowLoopback,
		// Logs only. The server-side external_read report is a later slice.
		OnExternalRead: func(host string) { log.Warn("egress: external read", "host", host) },
	}
	go func() { defer close(re.done); _ = re.proxy.Serve(ln) }()
	return re, nil
}

// Close stops the proxy, removes the socket and directory, and writes the
// destination list to the task log. Safe on nil and on repeat.
func (re *runEgress) Close() {
	if re == nil {
		return
	}
	re.mu.Lock()
	defer re.mu.Unlock()
	if re.closed {
		return
	}
	re.closed = true
	_ = re.ln.Close()
	<-re.done
	re.proxy.Shutdown() // closes open tunnels and waits for handlers: the list below is final
	_ = os.RemoveAll(re.dir)
	for _, d := range re.proxy.Dests() {
		re.log.Info("egress destination", "host", d.Host, "port", d.Port,
			"allowed", d.Allowed, "external", d.External, "reason", d.Reason)
	}
}
