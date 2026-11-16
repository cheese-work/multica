package daemon

import (
	"context"
	"net/http"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/readproxy"
)

// RunReadProxyConfig is everything one elevated run's read proxy needs.
type RunReadProxyConfig struct {
	IssueID, TaskID string
	Epoch           int64 // the epoch the run was claimed at
	GraphQLURL      string
	RESTURL         string
	HTTP            *http.Client
	Token           func(context.Context) (string, error)
	Admit           readproxy.Admitter
	Approve         readproxy.Approver
	Broker          runFenceBroker
	Restrict        func()
	StopRun         func() error
	Attempts        int
	Backoff         time.Duration
	Wait            time.Duration
	Alert           func(error)
}

type runFenceBroker interface {
	Fence()
	Drain(context.Context) error
	OwnMessage(sha string, msg []byte) bool
}

// NewRunReadProxy builds one run's read proxy. A read that asks for a downgrade
// runs the real handshake: RunFence (local restrict, broker fence, run stop,
// launch scopes ended, drain), then the server fence and delivery through the
// client bound to this task. The proxy releases bytes only when all of it
// succeeded.
func (c *Client) NewRunReadProxy(cfg RunReadProxyConfig) *readproxy.Proxy {
	p := &readproxy.Proxy{IssueID: cfg.IssueID, TaskID: cfg.TaskID, GraphQLURL: cfg.GraphQLURL,
		RESTURL: cfg.RESTURL, HTTP: cfg.HTTP, Token: cfg.Token, Admit: cfg.Admit, Approve: cfg.Approve,
		DowngradeWait: cfg.Wait}
	if cfg.Broker != nil {
		p.Own = cfg.Broker
		p.Downgrade = &Downgrader{
			Server: c.TaskDowngradeServer(cfg.TaskID),
			Fence: RunFence{
				Broker:          cfg.Broker,
				Restrict:        cfg.Restrict,
				StopRun:         cfg.StopRun,
				EndLaunchScopes: func(ctx context.Context) error { return c.EndLaunchScopes(ctx, cfg.TaskID) },
			},
			Epoch: cfg.Epoch, Attempts: cfg.Attempts, Backoff: cfg.Backoff, Alert: cfg.Alert,
		}
	}
	return p
}

// The run's real broker is what the proxy fences.
var _ runFenceBroker = (*runBroker)(nil)
