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
	// LaunchScopes is true when the task holds docker or device: scopes. Those
	// are granted for the whole run, so the fence must stop the run (StopRun)
	// and report the scopes ended; the requester dies with them and the text
	// goes through OnDowngrade to a successor turn instead.
	LaunchScopes bool
	StopRun      func() error
	OnDowngrade  func(readproxy.Result, bool)
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
// fence, drain), then delivery through the client bound to this task. The
// proxy releases bytes only when all of it succeeded.
//
// A task with no launch scope keeps its run alive (KeepRun), so the requesting
// process receives the bytes itself. A task with one cannot: its fence stops
// the run and reports the launch scopes ended (the requester dies), so the
// handshake runs detached from the request and the outcome goes to OnDowngrade.
func (c *Client) NewRunReadProxy(cfg RunReadProxyConfig) *readproxy.Proxy {
	p := &readproxy.Proxy{IssueID: cfg.IssueID, TaskID: cfg.TaskID, GraphQLURL: cfg.GraphQLURL,
		RESTURL: cfg.RESTURL, HTTP: cfg.HTTP, Token: cfg.Token, Admit: cfg.Admit, Approve: cfg.Approve,
		DowngradeWait: cfg.Wait, OnDowngrade: cfg.OnDowngrade}
	if cfg.Broker != nil {
		p.Own = cfg.Broker
		fence := RunFence{Broker: cfg.Broker, Restrict: cfg.Restrict, KeepRun: true}
		if cfg.LaunchScopes {
			fence = RunFence{
				Broker:          cfg.Broker,
				Restrict:        cfg.Restrict,
				StopRun:         cfg.StopRun,
				EndLaunchScopes: func(ctx context.Context) error { return c.EndLaunchScopes(ctx, cfg.TaskID) },
			}
			p.DetachDowngrade = true
		}
		p.Downgrade = &Downgrader{
			Server: c.TaskDowngradeServer(cfg.TaskID),
			Fence:  fence,
			Epoch:  cfg.Epoch, Attempts: cfg.Attempts, Backoff: cfg.Backoff, Alert: cfg.Alert,
		}
	}
	return p
}

// The run's real broker is what the proxy fences.
var _ runFenceBroker = (*runBroker)(nil)
