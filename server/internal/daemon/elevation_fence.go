package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/broker"
)

// Daemon side of the revocation fence (CHE-2216 plan 3.8, slice B4b).

// elevationAuthority is the broker's Authority for one task and epoch.
type elevationAuthority struct {
	c      *Client
	taskID string
	epoch  int64
}

// ElevationAuthority binds the task and the epoch the run was claimed at, so
// the server refuses every authorize that comes after a fence.
func (c *Client) ElevationAuthority(taskID string, epoch int64) broker.Authority {
	return elevationAuthority{c, taskID, epoch}
}

func (a elevationAuthority) Authorize(ctx context.Context, scope, inputHash string) (string, error) {
	var resp struct {
		OpID string `json:"op_id"`
	}
	err := a.c.postJSON(ctx, fmt.Sprintf("/api/daemon/tasks/%s/elevation/authorize", a.taskID),
		map[string]any{"epoch": a.epoch, "scope": scope, "input_hash": inputHash}, &resp)
	if err == nil && resp.OpID == "" {
		err = errors.New("authorize: no op_id in the response")
	}
	return resp.OpID, err
}

func (a elevationAuthority) Complete(ctx context.Context, opID, result string) error {
	return a.c.postJSON(ctx, fmt.Sprintf("/api/daemon/tasks/%s/elevation/ops/%s/complete", a.taskID, opID),
		map[string]any{"result": result}, nil)
}

// ElevationState reads where the task's fence stands.
func (c *Client) ElevationState(ctx context.Context, taskID string) (state string, epoch int64, err error) {
	var resp struct {
		State string `json:"state"`
		Epoch int64  `json:"epoch"`
	}
	err = c.getJSON(ctx, fmt.Sprintf("/api/daemon/tasks/%s/elevation", taskID), &resp)
	return resp.State, resp.Epoch, err
}

// EndLaunchScopes tells the server the run's docker and device: processes exited.
func (c *Client) EndLaunchScopes(ctx context.Context, taskID string) error {
	return c.postJSON(ctx, fmt.Sprintf("/api/daemon/tasks/%s/elevation/launch-scopes-ended", taskID), nil, nil)
}

// RunFence is the daemon's answer to a fence for one elevated run.
type RunFence struct {
	Broker interface {
		Fence()
		Drain(context.Context) error
	}
	// Restrict switches the run's local policy to restricted. It runs first and
	// needs nothing from the server: a failed server fence still restricts.
	Restrict func()
	// StopRun ends the run's process tree and returns once it has exited. An
	// error means a process may still be alive.
	StopRun func() error
	// EndLaunchScopes reports that exit to the server.
	EndLaunchScopes func(context.Context) error
}

// Run restricts, stops new and unsent operations, ends the run, and waits for
// started operations to report. Launch scopes are reported ended only after the
// processes exited; a run that will not stop leaves the fence open (fail closed).
func (f RunFence) Run(ctx context.Context) error {
	f.Restrict()
	f.Broker.Fence()
	var stopErr, endErr error
	if stopErr = f.StopRun(); stopErr == nil {
		endErr = f.EndLaunchScopes(ctx)
	}
	return errors.Join(stopErr, endErr, f.Broker.Drain(ctx))
}

// WatchFence polls the server until the task leaves its claimed epoch's
// elevated state, then calls onFence once. A failed poll is retried; the
// per-operation authorize already refuses without the server.
func WatchFence(ctx context.Context, state func(context.Context) (string, int64, error), epoch int64, every time.Duration, onFence func()) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s, e, err := state(ctx); err == nil && (s != "elevated" || e != epoch) {
				onFence()
				return
			}
		}
	}
}
