package daemon

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/readproxy"
	"github.com/multica-ai/multica/server/internal/daemon/repocache"
)

// runReadProxy is one elevated run's read proxy: the listener beside the run's
// broker socket and the read credential it holds.
type runReadProxy struct {
	Socket string

	ln         net.Listener
	src        interface{ Close(context.Context) error }
	restricted atomic.Bool
	once       sync.Once
}

// Restricted reports whether the run's local policy was switched to restricted.
func (p *runReadProxy) Restricted() bool { return p.restricted.Load() }

// Close stops the listener and revokes the read token. Safe on nil and repeat.
func (p *runReadProxy) Close() {
	if p == nil {
		return
	}
	p.once.Do(func() {
		_ = p.ln.Close()
		_ = os.Remove(p.Socket)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = p.src.Close(ctx)
	})
}

// denyAdmit admits nothing. The server's admission check takes the object's
// numeric id and author id, which the proxy's read does not carry yet (CHE-2214
// maps it), so until then every read stays a stub unless it asks for the
// downgrade. Fail closed.
type denyAdmit struct{}

func (denyAdmit) Admitted(context.Context, readproxy.Check) (bool, error) { return false, nil }

// hasLaunchScope reports whether the task holds a docker or device: scope,
// mirroring the server's isLaunchScope.
func hasLaunchScope(scopes []string) bool {
	for _, sc := range scopes {
		if sc == "docker" || strings.HasPrefix(sc, "device:") {
			return true
		}
	}
	return false
}

// startRunReadProxy is the per-run launch hook next to applyRunBroker. Only an
// elevated run that has a broker gets a proxy: its socket sits beside the
// broker's and the run's env names it. The run itself is never stopped: the
// process that asks for the downgrade is the one that receives the response.
// A task with a launch scope is refused the handshake instead (see
// RunReadProxyConfig.LaunchScopes). The caller closes the proxy when the run
// ends. An elevated run without a proxy has no GitHub read path at all, which
// is the safe state.
func startRunReadProxy(c *Client, task Task, rb *runBroker, agentEnv map[string]string, log *slog.Logger) (*runReadProxy, error) {
	if rb == nil || task.EffectiveProfile() != "elevated" {
		return nil, nil
	}
	slug := strings.TrimPrefix(repocache.RepoIdentity(task.PinnedRepo), "github.com/")
	repoID, ok := rb.cfg.RepoIDs[slug]
	if !ok {
		return nil, errors.New("read proxy: no GitHub repository ID configured for the pinned repository")
	}
	sock := filepath.Join(filepath.Dir(rb.Socket), "r.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	api := strings.TrimRight(rb.cfg.APIBase, "/")
	if api == "" {
		api = "https://api.github.com"
	}
	src := rb.tokens.NewReadSource(repoID)
	p := &runReadProxy{Socket: sock, ln: ln, src: src}
	proxy := c.NewRunReadProxy(RunReadProxyConfig{
		IssueID: task.IssueID, TaskID: task.ID, Epoch: task.Epoch,
		GraphQLURL: api + "/graphql", RESTURL: api,
		Token:        src.Token,
		Admit:        denyAdmit{},
		Broker:       rb,
		LaunchScopes: hasLaunchScope(task.Scopes),
		Restrict:     func() { p.restricted.Store(true); log.Warn("elevated run restricted", "task_id", task.ID) },
		Attempts:     3, Backoff: time.Second,
		Alert: func(err error) { log.Error("downgrade handshake", "task_id", task.ID, "error", err) },
	})
	go func() { _ = proxy.Serve(ln) }()
	agentEnv[readproxy.SocketEnv] = sock
	return p, nil
}
