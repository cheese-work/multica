package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Daemon side of the downgrade handshake (CHE-2216 plan 3.4 and 3.8, slice B4c).

// StartDowngrade asks the server to fence the task for a downgrade. The epoch is
// the one the run was claimed at.
func (c *Client) StartDowngrade(ctx context.Context, taskID string, epoch int64) error {
	return c.postJSON(ctx, fmt.Sprintf("/api/daemon/tasks/%s/elevation/downgrade", taskID), map[string]any{"epoch": epoch}, nil)
}

// DeliverDowngrade asks the server to confirm delivery. It answers 200 only
// after the fence completed and the task and issue carry the taint.
func (c *Client) DeliverDowngrade(ctx context.Context, taskID string) error {
	return c.postJSON(ctx, fmt.Sprintf("/api/daemon/tasks/%s/elevation/downgrade/deliver", taskID), map[string]any{}, nil)
}

// DowngradeServer is the server half of the handshake for one task.
type DowngradeServer interface {
	StartDowngrade(ctx context.Context, epoch int64) error
	DeliverDowngrade(ctx context.Context) error
}

type taskDowngradeServer struct {
	c      *Client
	taskID string
}

func (s taskDowngradeServer) StartDowngrade(ctx context.Context, epoch int64) error {
	return s.c.StartDowngrade(ctx, s.taskID, epoch)
}

func (s taskDowngradeServer) DeliverDowngrade(ctx context.Context) error {
	return s.c.DeliverDowngrade(ctx, s.taskID)
}

// TaskDowngradeServer binds the client to one task.
func (c *Client) TaskDowngradeServer(taskID string) DowngradeServer {
	return taskDowngradeServer{c, taskID}
}

const (
	defaultDowngradeAttempts = 3
	defaultDowngradeBackoff  = 500 * time.Millisecond
)

// Downgrader runs the handshake for one elevated run. It is the read proxy's
// Downgrader.
type Downgrader struct {
	Server DowngradeServer
	// Fence is the run's RunFence: restrict locally, stop new and unsent
	// operations, fence the server, end launch-scope processes and report them
	// ended, wait for started operations.
	Fence interface {
		RunWithServerFence(ctx context.Context, serverFence func(context.Context) error) error
	}
	Epoch int64 // the epoch the run was claimed at
	// Attempts and Backoff bound the retries of the server fence call.
	Attempts int
	Backoff  time.Duration
	// Alert reports every failed handshake. It must not block.
	Alert func(error)
}

// Downgrade returns nil only when it is safe to deliver withheld bytes. The
// local restriction runs first and needs nothing from the server, so the run is
// restricted even if every server call fails. The server fence (retried) runs
// inside the run fence, before launch scopes are reported ended, because the
// server accepts that report only from a fenced task. Delivery is asked for
// only after the whole run fence succeeded. Any failure alerts and returns an
// error: the proxy keeps the stub.
func (d *Downgrader) Downgrade(ctx context.Context) error {
	if err := d.Fence.RunWithServerFence(ctx, d.startServerFence); err != nil {
		return d.fail(fmt.Errorf("downgrade fence: %w", err))
	}
	if err := d.Server.DeliverDowngrade(ctx); err != nil {
		return d.fail(fmt.Errorf("downgrade delivery: %w", err))
	}
	return nil
}

func (d *Downgrader) startServerFence(ctx context.Context) error {
	attempts, backoff := d.Attempts, d.Backoff
	if attempts <= 0 {
		attempts = defaultDowngradeAttempts
	}
	if backoff <= 0 {
		backoff = defaultDowngradeBackoff
	}
	var err error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return errors.Join(err, ctx.Err())
			case <-time.After(backoff):
			}
		}
		if err = d.Server.StartDowngrade(ctx, d.Epoch); err == nil {
			return nil
		}
	}
	return err
}

func (d *Downgrader) fail(err error) error {
	if d.Alert != nil {
		d.Alert(err)
	}
	return err
}
