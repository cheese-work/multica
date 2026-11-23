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
	"github.com/multica-ai/multica/server/pkg/agent"
)

// runReadProxy is one elevated run's read proxy: the listener beside the run's
// broker socket and the read credential it holds.
type runReadProxy struct {
	Socket string

	ln         net.Listener
	src        interface{ Close(context.Context) error }
	restricted atomic.Bool
	// successor builds the backend of the successor turn, without the run's
	// launch grants. Nil: a launch-scope run cannot be downgraded.
	successor restrictedLaunch
	once      sync.Once

	// The run's context and the agent processes under it, for a task whose
	// fence stops the run (launch scopes). See run_downgrade_successor.go.
	mu      sync.Mutex
	outer   context.Context
	runCtx  context.Context
	cancel  context.CancelFunc
	tracker *runTracker
	stopped bool
	outcome chan downgradeOutcome
}

// Restricted reports whether the run's local policy was switched to restricted.
func (p *runReadProxy) Restricted() bool { return p.restricted.Load() }

// Close stops the listener and revokes the read token. Safe on nil and repeat.
func (p *runReadProxy) Close() {
	if p == nil {
		return
	}
	p.once.Do(func() {
		p.mu.Lock()
		if p.cancel != nil {
			p.cancel()
		}
		p.mu.Unlock()
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

// restrictedLaunch builds the successor turn's backend and options without the
// run's launch grants. See restrictedSuccessorLaunch.
type restrictedLaunch func(agent.Backend, agent.ExecOptions) (agent.Backend, agent.ExecOptions, error)

// restrictedSuccessorLaunch is nil until agent runs start through sandbox.Launcher
// (the launcher wiring that makes Daemon.isolated true): only then is there a launch that
// provably lacks the run's docker and device access. Until then a launch-scope
// task is refused its downgrade rather than handed the text in a process that
// still holds those grants.
var restrictedSuccessorLaunch restrictedLaunch

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
// broker's and the run's env names it. A task with no launch scope keeps its
// run: the process that asks for the downgrade receives the response. A task
// with one has its run stopped by the fence and gets the text in a successor
// turn (executeWithDowngrade). The caller closes the proxy when the run
// ends. An elevated run without a proxy has no GitHub read path at all, which
// is the safe state.
func startRunReadProxy(c *Client, task Task, rb *runBroker, agentEnv map[string]string, restricted restrictedLaunch, log *slog.Logger) (*runReadProxy, error) {
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
	p := &runReadProxy{Socket: sock, ln: ln, src: src, successor: restricted, outcome: make(chan downgradeOutcome, 1)}
	proxy := c.NewRunReadProxy(RunReadProxyConfig{
		IssueID: task.IssueID, TaskID: task.ID, Epoch: task.Epoch,
		GraphQLURL: api + "/graphql", RESTURL: api,
		Token:               src.Token,
		Admit:               denyAdmit{},
		Broker:              rb,
		LaunchScopes:        hasLaunchScope(task.Scopes),
		SuccessorRestricted: restricted != nil,
		StopRun:             p.StopRun,
		OnDowngrade:         p.noteDowngrade,
		Restrict:            func() { p.restricted.Store(true); log.Warn("elevated run restricted", "task_id", task.ID) },
		Attempts:            3, Backoff: time.Second,
		Alert: func(err error) { log.Error("downgrade handshake", "task_id", task.ID, "error", err) },
	})
	go func() { _ = proxy.Serve(ln) }()
	agentEnv[readproxy.SocketEnv] = sock
	return p, nil
}
