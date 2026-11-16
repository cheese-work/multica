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

// runStopTimeout bounds the wait for the run's agent process to exit after
// the fence cancels it.
const runStopTimeout = 30 * time.Second

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

// startRunReadProxy is the per-run launch hook next to applyRunBroker. Only an
// elevated run that has a broker gets a proxy: its socket sits beside the
// broker's, the run's env names it, and the returned context is the run's:
// the fence stops the run by cancelling it and waiting for its agent process
// (every executeAndDrain under that context) to exit. The caller closes the
// proxy when the run ends. An elevated run without a proxy has no GitHub read
// path at all, which is the safe state.
func startRunReadProxy(ctx context.Context, c *Client, task Task, rb *runBroker, agentEnv map[string]string, log *slog.Logger) (context.Context, *runReadProxy, error) {
	if rb == nil || task.EffectiveProfile() != "elevated" {
		return ctx, nil, nil
	}
	slug := strings.TrimPrefix(repocache.RepoIdentity(task.PinnedRepo), "github.com/")
	repoID, ok := rb.cfg.RepoIDs[slug]
	if !ok {
		return ctx, nil, errors.New("read proxy: no GitHub repository ID configured for the pinned repository")
	}
	sock := filepath.Join(filepath.Dir(rb.Socket), "r.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return ctx, nil, err
	}
	api := strings.TrimRight(rb.cfg.APIBase, "/")
	if api == "" {
		api = "https://api.github.com"
	}
	src := rb.tokens.NewReadSource(repoID)
	p := &runReadProxy{Socket: sock, ln: ln, src: src}

	runCtx, cancel := context.WithCancel(ctx)
	tracker := &runTracker{}
	runCtx = withRunTracker(runCtx, tracker)
	proxy := c.NewRunReadProxy(RunReadProxyConfig{
		IssueID: task.IssueID, TaskID: task.ID, Epoch: task.Epoch,
		GraphQLURL: api + "/graphql", RESTURL: api,
		Token:    src.Token,
		Admit:    denyAdmit{},
		Broker:   rb,
		Restrict: func() { p.restricted.Store(true); log.Warn("elevated run restricted", "task_id", task.ID) },
		StopRun:  func() error { cancel(); return tracker.idle(runStopTimeout) },
		Attempts: 3, Backoff: time.Second,
		Alert: func(err error) { log.Error("downgrade handshake", "task_id", task.ID, "error", err) },
	})
	go func() { _ = proxy.Serve(ln) }()
	agentEnv[readproxy.SocketEnv] = sock
	return runCtx, p, nil
}
