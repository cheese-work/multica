package ghstack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
)

// Runner runs one link job. *Executor satisfies it.
type Runner interface {
	Run(ctx context.Context, j Job) error
}

// Links answers which PRs may trigger a job. Only PRs linked to a Multica issue do.
type Links interface {
	IsLinked(ctx context.Context, j Job) (bool, error)
	// ListLinkedStacked returns up to limit linked open PRs whose base is the
	// head branch of another open PR, in a stable order after the given PR.
	ListLinkedStacked(ctx context.Context, after Job, limit int) ([]Job, error)
}

// Backoff is the per-installation rate-limit state shared with the snapshot
// pipeline (*ghsnapshot.Manager), so a limit hit by either pauses both.
type Backoff interface {
	RateLimitPause(installationID int64) time.Duration
	ExtendRateLimit(installationID int64, d time.Duration)
}

const (
	defaultWorkers   = 2
	defaultQueueSize = 256
	defaultSweepRows = 100
	defaultTimeout   = 2 * time.Minute
)

// Dispatcher turns webhook and sweep triggers into bounded background link
// jobs. Enqueue never blocks and never returns an error, so a failing stacks
// API cannot change a webhook response; failures become reports (in the
// executor) and logs (here).
type Dispatcher struct {
	Runner    Runner
	Links     Links
	Backoff   Backoff // optional
	Workers   int     // default 2
	QueueSize int     // default 256
	SweepRows int     // max PRs per sweep pass, default 100
	Timeout   time.Duration
	Log       *slog.Logger

	mu       sync.Mutex
	queue    chan Job
	active   map[string]bool // queued or running: coalesces
	running  map[string]bool
	trailing map[string]bool // one event arrived while running: rerun once
	after    Job             // sweep cursor
	ctx      context.Context
}

func (d *Dispatcher) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

func jobKey(j Job) string {
	return fmt.Sprintf("%d/%s/%s#%d", j.InstallationID, strings.ToLower(j.Owner), strings.ToLower(j.Repo), j.PR)
}

// Start launches the workers under ctx. Safe to call once; a nil Dispatcher is inert.
func (d *Dispatcher) Start(ctx context.Context) {
	if d == nil {
		return
	}
	d.mu.Lock()
	if d.queue != nil {
		d.mu.Unlock()
		return
	}
	size := d.QueueSize
	if size <= 0 {
		size = defaultQueueSize
	}
	d.queue = make(chan Job, size)
	d.active, d.running, d.trailing = map[string]bool{}, map[string]bool{}, map[string]bool{}
	d.ctx = ctx
	d.mu.Unlock()
	workers := d.Workers
	if workers <= 0 {
		workers = defaultWorkers
	}
	for i := 0; i < workers; i++ {
		go d.worker(ctx)
	}
}

// Enqueue never blocks and never fails: a full queue drops the job, and the
// sweep recovers it. A second event for a PR that is queued is coalesced; one
// that arrives while the PR's job runs leaves one trailing rerun.
func (d *Dispatcher) Enqueue(j Job) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.queue == nil {
		return
	}
	k := jobKey(j)
	if d.active[k] {
		if d.running[k] {
			d.trailing[k] = true
		}
		return
	}
	select {
	case d.queue <- j:
		d.active[k] = true
	default:
		d.log().Warn("ghstack: link queue full, dropping job", "repo", j.Owner+"/"+j.Repo, "pr", j.PR)
	}
}

func (d *Dispatcher) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-d.queue:
			d.handle(ctx, j)
		}
	}
}

// handle runs one job: pause while the installation is rate limited, drop an
// unlinked PR, run, and log (never return) a failure.
func (d *Dispatcher) handle(ctx context.Context, j Job) {
	k := jobKey(j)
	if d.Backoff != nil {
		if pause := d.Backoff.RateLimitPause(j.InstallationID); pause > 0 {
			// The address stays active while the timer owns it, so events keep
			// coalescing and no worker is held.
			time.AfterFunc(pause, func() { d.requeueAfterPause(j) })
			return
		}
	}
	d.mu.Lock()
	d.running[k] = true
	d.mu.Unlock()
	d.runOne(ctx, j)
	d.mu.Lock()
	delete(d.running, k)
	again := d.trailing[k]
	delete(d.trailing, k)
	delete(d.active, k)
	d.mu.Unlock()
	if again {
		d.Enqueue(j)
	}
}

func (d *Dispatcher) requeueAfterPause(j Job) {
	select {
	case <-d.ctx.Done():
		return
	default:
	}
	select {
	case d.queue <- j:
	default:
		d.mu.Lock()
		delete(d.active, jobKey(j))
		d.mu.Unlock()
		d.log().Warn("ghstack: link queue full, dropping rate-limited job", "repo", j.Owner+"/"+j.Repo, "pr", j.PR)
	}
}

func (d *Dispatcher) runOne(ctx context.Context, j Job) {
	defer func() {
		if r := recover(); r != nil {
			d.log().Error("ghstack: link job panicked", "repo", j.Owner+"/"+j.Repo, "pr", j.PR, "panic", fmt.Sprint(r))
		}
	}()
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	linked, err := d.Links.IsLinked(ctx, j)
	if err != nil {
		d.log().Warn("ghstack: link check failed", "repo", j.Owner+"/"+j.Repo, "pr", j.PR, "err", err.Error())
		return
	}
	if !linked {
		return
	}
	err = d.Runner.Run(ctx, j)
	if err == nil {
		return
	}
	var rl *ghsnapshot.RateLimitError
	if errors.As(err, &rl) && d.Backoff != nil {
		d.Backoff.ExtendRateLimit(j.InstallationID, rl.RetryAfter)
	}
	d.log().Warn("ghstack: link job failed", "repo", j.Owner+"/"+j.Repo, "pr", j.PR, "err", err.Error())
}

// Sweep enqueues one bounded pass over linked open stacked PRs. The cursor
// rotates across passes, so a fixed first page cannot starve later PRs; it
// wraps to the start when a pass finds nothing after it.
func (d *Dispatcher) Sweep(ctx context.Context) {
	if d == nil || d.Links == nil {
		return
	}
	rows := d.SweepRows
	if rows <= 0 {
		rows = defaultSweepRows
	}
	d.mu.Lock()
	after := d.after
	d.mu.Unlock()
	jobs, err := d.Links.ListLinkedStacked(ctx, after, rows)
	if err == nil && len(jobs) == 0 && after != (Job{}) {
		after = Job{}
		jobs, err = d.Links.ListLinkedStacked(ctx, after, rows)
	}
	if err != nil {
		d.log().Warn("ghstack: sweep query failed", "err", err.Error())
		return
	}
	d.mu.Lock()
	if len(jobs) > 0 {
		d.after = jobs[len(jobs)-1]
	} else {
		d.after = Job{}
	}
	d.mu.Unlock()
	for _, j := range jobs {
		d.Enqueue(j)
	}
}
