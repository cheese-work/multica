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
	// LaunchScopes is true when the task holds docker or device: scopes. Ending
	// those processes also ends the one that asked for the downgrade, so such a
	// task gets no handshake at all and every read stays a stub.
	LaunchScopes bool
	Attempts     int
	Backoff      time.Duration
	Wait         time.Duration
	Alert        func(error)
}

type runFenceBroker interface {
	Fence()
	Drain(context.Context) error
	OwnMessage(sha string, msg []byte) bool
}

// NewRunReadProxy builds one run's read proxy. A read that asks for a downgrade
// runs the real handshake: RunFence (local restrict, broker fence, server
// fence, drain) with the run kept alive, then delivery through the client bound
// to this task. The requesting process must survive to receive the bytes, which
// the server allows only for a task with no launch scope; a task with one is
// refused the handshake (stub, nothing started). The proxy releases bytes only
// when all of it succeeded.
func (c *Client) NewRunReadProxy(cfg RunReadProxyConfig) *readproxy.Proxy {
	p := &readproxy.Proxy{IssueID: cfg.IssueID, TaskID: cfg.TaskID, GraphQLURL: cfg.GraphQLURL,
		RESTURL: cfg.RESTURL, HTTP: cfg.HTTP, Token: cfg.Token, Admit: cfg.Admit, Approve: cfg.Approve,
		DowngradeWait: cfg.Wait}
	if cfg.Broker != nil && !cfg.LaunchScopes {
		p.Own = cfg.Broker
		p.Downgrade = &Downgrader{
			Server: c.TaskDowngradeServer(cfg.TaskID),
			Fence: RunFence{
				Broker:   cfg.Broker,
				Restrict: cfg.Restrict,
				KeepRun:  true,
			},
			Epoch: cfg.Epoch, Attempts: cfg.Attempts, Backoff: cfg.Backoff, Alert: cfg.Alert,
		}
	}
	return p
}

// The run's real broker is what the proxy fences.
var _ runFenceBroker = (*runBroker)(nil)
