package ghstack

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
)

// fakeLinks is the link source: linked PR numbers, plus a stacked list for the sweep.
type fakeLinks struct {
	mu      sync.Mutex
	linked  map[int]bool
	stacked []Job
	afters  []Job
	limits  []int
}

func (l *fakeLinks) IsLinked(_ context.Context, j Job) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.linked[j.PR], nil
}

func (l *fakeLinks) ListLinkedStacked(_ context.Context, after Job, limit int) ([]Job, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.afters = append(l.afters, after)
	l.limits = append(l.limits, limit)
	var out []Job
	for _, j := range l.stacked {
		if j.PR > after.PR && len(out) < limit {
			out = append(out, j)
		}
	}
	return out, nil
}

// doneRunner signals every finished Run.
type doneRunner struct {
	Runner
	done chan Job
	err  error
}

func (r *doneRunner) Run(ctx context.Context, j Job) error {
	err := r.Runner.Run(ctx, j)
	if r.err != nil {
		err = r.err
	}
	r.done <- j
	return err
}

func waitJob(t *testing.T, ch chan Job) Job {
	t.Helper()
	select {
	case j := <-ch:
		return j
	case <-time.After(3 * time.Second):
		t.Fatal("job did not run")
		return Job{}
	}
}

func noJob(t *testing.T, ch chan Job) {
	t.Helper()
	select {
	case j := <-ch:
		t.Fatalf("unexpected job %+v", j)
	case <-time.After(150 * time.Millisecond):
	}
}

func startDispatcher(t *testing.T, f *fakeGH, links *fakeLinks, r Resolution) (*Dispatcher, *doneRunner, *recReporter) {
	t.Helper()
	e, rep, _ := setup(t, f, r)
	dr := &doneRunner{Runner: e, done: make(chan Job, 16)}
	d := &Dispatcher{Runner: dr, Links: links, SweepRows: 2}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d.Start(ctx)
	return d, dr, rep
}

func TestDispatcherLinkedJobWritesInLinkMode(t *testing.T) {
	f := chain2()
	d, dr, _ := startDispatcher(t, f, &fakeLinks{linked: map[int]bool{2: true}}, res(ModeLink))
	d.Enqueue(job(2))
	waitJob(t, dr.done)
	if f.writeCount() != 1 {
		t.Fatalf("writes = %v, want one create", f.writes)
	}
}

func TestDispatcherUnlinkedPRRunsNoJob(t *testing.T) {
	f := chain2()
	d, dr, _ := startDispatcher(t, f, &fakeLinks{linked: map[int]bool{}}, res(ModeLink))
	d.Enqueue(job(2))
	noJob(t, dr.done)
	if f.writeCount() != 0 {
		t.Fatalf("unlinked PR wrote: %v", f.writes)
	}
}

// Webhook (Enqueue) and sweep both honour off and report: never a write.
func TestDispatcherWebhookAndSweepHonourOffAndReport(t *testing.T) {
	for _, tc := range []struct {
		mode Mode
		want []Code
	}{{ModeOff, nil}, {ModeReport, []Code{OutcomeWouldCreate}}} {
		for _, via := range []string{"webhook", "sweep"} {
			t.Run(string(tc.mode)+"/"+via, func(t *testing.T) {
				f := chain2()
				links := &fakeLinks{linked: map[int]bool{2: true}, stacked: []Job{job(2)}}
				d, dr, rep := startDispatcher(t, f, links, res(tc.mode))
				if via == "webhook" {
					d.Enqueue(job(2))
				} else {
					d.Sweep(context.Background())
				}
				waitJob(t, dr.done)
				if f.writeCount() != 0 {
					t.Fatalf("%s mode wrote: %v", tc.mode, f.writes)
				}
				if got := rep.codes(); !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("reports = %v, want %v", got, tc.want)
				}
			})
		}
	}
}

// A stacks API failure is a report or a logged error, never a crash; the next job still runs.
func TestDispatcherSurvivesStacksAPIFailure(t *testing.T) {
	f := chain2()
	f.stacksCode = 500
	d, dr, _ := startDispatcher(t, f, &fakeLinks{linked: map[int]bool{2: true, 3: true}}, res(ModeLink))
	d.Enqueue(job(2))
	waitJob(t, dr.done)
	d.Enqueue(job(3))
	waitJob(t, dr.done)
}

func TestDispatcherEnqueueNeverBlocksWhenQueueIsFull(t *testing.T) {
	block := make(chan struct{})
	r := runnerFunc(func(ctx context.Context, j Job) error { <-block; return nil })
	d := &Dispatcher{Runner: r, Links: &fakeLinks{linked: map[int]bool{1: true, 2: true, 3: true, 4: true}}, Workers: 1, QueueSize: 1}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer close(block)
	d.Start(ctx)
	finished := make(chan struct{})
	go func() {
		for n := 1; n <= 50; n++ {
			d.Enqueue(job(n))
		}
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("Enqueue blocked on a full queue")
	}
}

func waitSignal(t *testing.T, ch chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("job did not start")
	}
}

type runnerFunc func(ctx context.Context, j Job) error

func (f runnerFunc) Run(ctx context.Context, j Job) error { return f(ctx, j) }

func TestDispatcherCoalescesAndRerunsWhenEventArrivesInFlight(t *testing.T) {
	var mu sync.Mutex
	var runs int
	started := make(chan struct{}, 4)
	release := make(chan struct{}, 4)
	r := runnerFunc(func(ctx context.Context, j Job) error {
		mu.Lock()
		runs++
		mu.Unlock()
		started <- struct{}{}
		<-release
		return nil
	})
	d := &Dispatcher{Runner: r, Links: &fakeLinks{linked: map[int]bool{2: true}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	d.Enqueue(job(2))
	waitSignal(t, started)
	d.Enqueue(job(2)) // arrives in flight: one trailing rerun
	d.Enqueue(job(2)) // coalesced into it
	release <- struct{}{}
	waitSignal(t, started)
	release <- struct{}{}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if runs != 2 {
		t.Fatalf("runs = %d, want 2 (in-flight + one trailing)", runs)
	}
}

func TestSweepIsBoundedAndRotates(t *testing.T) {
	links := &fakeLinks{linked: map[int]bool{1: true, 2: true, 3: true}, stacked: []Job{job(1), job(2), job(3)}}
	var mu sync.Mutex
	var ran []int
	r := runnerFunc(func(ctx context.Context, j Job) error { mu.Lock(); ran = append(ran, j.PR); mu.Unlock(); return nil })
	d := &Dispatcher{Runner: r, Links: links, SweepRows: 2}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	d.Sweep(ctx)
	time.Sleep(150 * time.Millisecond)
	d.Sweep(ctx)
	time.Sleep(150 * time.Millisecond)
	links.mu.Lock()
	defer links.mu.Unlock()
	if !reflect.DeepEqual(links.limits, []int{2, 2}) {
		t.Fatalf("sweep limits = %v, want [2 2]", links.limits)
	}
	if len(links.afters) != 2 || links.afters[1].PR != 2 {
		t.Fatalf("second pass must start after the last PR of the first: %+v", links.afters)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ran) != 3 {
		t.Fatalf("ran = %v, want PRs 1,2 then 3", ran)
	}
}

type fakeBackoff struct {
	mu     sync.Mutex
	until  time.Time
	extend []time.Duration
}

func (b *fakeBackoff) RateLimitPause(int64) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Until(b.until)
}
func (b *fakeBackoff) ExtendRateLimit(_ int64, d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.extend = append(b.extend, d)
}

func TestDispatcherReusesRateLimitBackoff(t *testing.T) {
	b := &fakeBackoff{until: time.Now().Add(200 * time.Millisecond)}
	ran := make(chan time.Time, 4)
	r := runnerFunc(func(ctx context.Context, j Job) error {
		ran <- time.Now()
		return &ghsnapshot.RateLimitError{RetryAfter: 7 * time.Second}
	})
	d := &Dispatcher{Runner: r, Links: &fakeLinks{linked: map[int]bool{2: true}}, Backoff: b}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	start := time.Now()
	d.Enqueue(job(2))
	select {
	case at := <-ran:
		if at.Sub(start) < 150*time.Millisecond {
			t.Fatalf("job ran %v after enqueue, inside the installation pause", at.Sub(start))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("job never ran after the pause")
	}
	time.Sleep(50 * time.Millisecond)
	b.mu.Lock()
	defer b.mu.Unlock()
	if !reflect.DeepEqual(b.extend, []time.Duration{7 * time.Second}) {
		t.Fatalf("backoff extend = %v, want the 429 retry-after", b.extend)
	}
}

func TestDispatcherRunnerErrorIsLoggedNotFatal(t *testing.T) {
	boom := errors.New("boom")
	f := chain2()
	d, dr, _ := startDispatcher(t, f, &fakeLinks{linked: map[int]bool{2: true, 3: true}}, res(ModeLink))
	dr.err = boom
	d.Enqueue(job(2))
	waitJob(t, dr.done)
	d.Enqueue(job(3))
	waitJob(t, dr.done)
}
