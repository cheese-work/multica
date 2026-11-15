package ghbudget

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var t0 = time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type fakeFetcher struct {
	calls atomic.Int32
	snap  Snapshot
	err   error
	delay time.Duration
	// during runs inside RateLimit, before it returns, to model a response
	// that lands while the refresh is in flight.
	during func()
}

func (f *fakeFetcher) RateLimit(context.Context, int64) (Snapshot, error) {
	f.calls.Add(1)
	time.Sleep(f.delay)
	if f.during != nil {
		f.during()
	}
	return f.snap, f.err
}

var instSeq atomic.Int64

type env struct {
	pool  *pgxpool.Pool
	clock *fakeClock
	fetch *fakeFetcher
	led   *Ledger
	inst  int64
}

func newEnv(t *testing.T) *env {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Skipf("database unavailable: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Skipf("database unavailable: %v", err)
	}
	inst := time.Now().UnixNano()%1_000_000_000*100 + instSeq.Add(1)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM gh_budget_reservation WHERE installation_id=$1`, inst)
		_, _ = pool.Exec(context.Background(), `DELETE FROM gh_installation_budget WHERE installation_id=$1`, inst)
		pool.Close()
	})
	clock := &fakeClock{t: t0}
	fetch := &fakeFetcher{snap: Snapshot{Remaining: 5000, ResetAt: t0.Add(50 * time.Minute)}}
	led := New(pool, fetch, WithClock(clock.Now), WithJitter(func() time.Duration { return 30 * time.Second }))
	return &env{pool: pool, clock: clock, fetch: fetch, led: led, inst: inst}
}

func (e *env) seed(t *testing.T, remaining int, reset, observed time.Time) {
	t.Helper()
	_, err := e.pool.Exec(context.Background(),
		`INSERT INTO gh_installation_budget (installation_id, remaining, reset_at, observed_at) VALUES ($1,$2,$3,$4)`,
		e.inst, remaining, reset, observed)
	if err != nil {
		t.Fatal(err)
	}
}

func (e *env) activeReservations(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM gh_budget_reservation WHERE installation_id=$1`, e.inst).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func deferred(t *testing.T, err error, reason Reason) *DeferredError {
	t.Helper()
	var d *DeferredError
	if !errors.As(err, &d) {
		t.Fatalf("err = %v, want DeferredError(%s)", err, reason)
	}
	if d.Reason != reason {
		t.Fatalf("reason = %s, want %s", d.Reason, reason)
	}
	return d
}

func TestEmptyLedgerRefreshesFromRateLimitThenReserves(t *testing.T) {
	e := newEnv(t)
	r, err := e.led.Reserve(context.Background(), e.inst, 10)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if e.fetch.calls.Load() != 1 {
		t.Fatalf("rate_limit calls = %d, want 1", e.fetch.calls.Load())
	}
	if e.activeReservations(t) != 1 {
		t.Fatal("reservation row missing")
	}
	if err := r.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.activeReservations(t) != 0 {
		t.Fatal("reservation not deleted on release")
	}
}

func TestEmptyLedgerWithFailedRefreshIsUnknownAndMakesNoReservation(t *testing.T) {
	e := newEnv(t)
	e.fetch.err = errors.New("boom")
	_, err := e.led.Reserve(context.Background(), e.inst, 10)
	d := deferred(t, err, ReasonUnknown)
	if !d.DueAt.After(t0) {
		t.Fatalf("DueAt %v must be after now", d.DueAt)
	}
	if e.activeReservations(t) != 0 {
		t.Fatal("no reservation expected")
	}
}

func TestExpiredLowLedgerRefreshesBeforeDeciding(t *testing.T) {
	e := newEnv(t)
	// 10 left, but the window reset 1 minute ago: the row is history.
	e.seed(t, 10, t0.Add(-time.Minute), t0.Add(-30*time.Minute))
	if _, err := e.led.Reserve(context.Background(), e.inst, 10); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if e.fetch.calls.Load() != 1 {
		t.Fatalf("rate_limit calls = %d, want 1", e.fetch.calls.Load())
	}
}

func TestStaleLedgerRefreshes(t *testing.T) {
	e := newEnv(t)
	e.seed(t, 2000, t0.Add(30*time.Minute), t0.Add(-6*time.Minute))
	if _, err := e.led.Reserve(context.Background(), e.inst, 10); err != nil {
		t.Fatal(err)
	}
	if e.fetch.calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", e.fetch.calls.Load())
	}
}

func TestFreshLedgerDoesNotCallRateLimit(t *testing.T) {
	e := newEnv(t)
	e.seed(t, 3000, t0.Add(30*time.Minute), t0.Add(-time.Minute))
	if _, err := e.led.Reserve(context.Background(), e.inst, 10); err != nil {
		t.Fatal(err)
	}
	if e.fetch.calls.Load() != 0 {
		t.Fatalf("calls = %d, want 0", e.fetch.calls.Load())
	}
}

func TestTwoReposOnOneInstallationShareTheFloor(t *testing.T) {
	e := newEnv(t)
	reset := t0.Add(30 * time.Minute)
	e.seed(t, 1600, reset, t0)
	ctx := context.Background()

	repoA, err := e.led.Reserve(ctx, e.inst, 60) // leaves 1540 remaining, at or above the 1500 floor
	if err != nil {
		t.Fatalf("repo A: %v", err)
	}
	// repo B, same installation: would leave 1480, below the 1500 floor.
	_, err = e.led.Reserve(ctx, e.inst, 60)
	d := deferred(t, err, ReasonBelowFloor)
	if d.DueAt.Before(reset) || d.DueAt.After(reset.Add(MaxJitter)) {
		t.Fatalf("DueAt %v not in [reset, reset+60s]", d.DueAt)
	}
	if e.activeReservations(t) != 1 {
		t.Fatalf("reservations = %d, want 1", e.activeReservations(t))
	}
	if err := repoA.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.led.Reserve(ctx, e.inst, 60); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

func TestReservationExpiresAfterTwoMinutes(t *testing.T) {
	e := newEnv(t)
	e.seed(t, 1600, t0.Add(30*time.Minute), t0)
	ctx := context.Background()
	if _, err := e.led.Reserve(ctx, e.inst, 60); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(ReservationTTL + time.Second)
	if _, err := e.led.Reserve(ctx, e.inst, 60); err != nil {
		t.Fatalf("expired reservation must not count: %v", err)
	}
}

// The P1 is scheduled; before it runs, other calls spent the budget down to
// the floor. It must defer to the reset, not cancel, and run after it.
func TestExhaustionJustBeforeScheduledCallDefersThenRunsAfterReset(t *testing.T) {
	e := newEnv(t)
	reset := t0.Add(20 * time.Minute)
	e.seed(t, 1500, reset, t0)
	ctx := context.Background()

	_, err := e.led.Reserve(ctx, e.inst, 1)
	d := deferred(t, err, ReasonBelowFloor)
	if d.DueAt.Before(reset) || d.DueAt.After(reset.Add(MaxJitter)) {
		t.Fatalf("DueAt %v not in [reset, reset+60s]", d.DueAt)
	}
	if e.fetch.calls.Load() != 0 {
		t.Fatal("must not call GitHub while below the floor")
	}

	e.clock.Advance(21 * time.Minute) // past reset
	e.fetch.snap = Snapshot{Remaining: 5000, ResetAt: t0.Add(80 * time.Minute)}
	if _, err := e.led.Reserve(ctx, e.inst, 1); err != nil {
		t.Fatalf("after reset: %v", err)
	}
	if e.fetch.calls.Load() != 1 {
		t.Fatalf("authoritative refresh calls = %d, want 1", e.fetch.calls.Load())
	}
}

func TestConcurrentReservesRefreshOnce(t *testing.T) {
	e := newEnv(t)
	e.fetch.delay = 100 * time.Millisecond
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.led.Reserve(context.Background(), e.inst, 1); err != nil {
				t.Errorf("Reserve: %v", err)
			}
		}()
	}
	wg.Wait()
	if e.fetch.calls.Load() != 1 {
		t.Fatalf("rate_limit calls = %d, want 1 (single flight)", e.fetch.calls.Load())
	}
}

func TestRefreshAtMostOncePerMinute(t *testing.T) {
	e := newEnv(t)
	e.fetch.err = errors.New("boom")
	ctx := context.Background()
	_, _ = e.led.Reserve(ctx, e.inst, 1)
	e.clock.Advance(30 * time.Second)
	_, err := e.led.Reserve(ctx, e.inst, 1)
	deferred(t, err, ReasonUnknown)
	if e.fetch.calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1 within 60s", e.fetch.calls.Load())
	}
	e.clock.Advance(31 * time.Second)
	_, _ = e.led.Reserve(ctx, e.inst, 1)
	if e.fetch.calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2 after 60s", e.fetch.calls.Load())
	}
}

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func TestObserveUpdatesLedgerFromHeaders(t *testing.T) {
	e := newEnv(t)
	e.seed(t, 4000, t0.Add(30*time.Minute), t0)
	reset := t0.Add(30 * time.Minute)
	if err := e.led.Observe(context.Background(), e.inst, hdr(
		"X-RateLimit-Remaining", "1510", "X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))); err != nil {
		t.Fatal(err)
	}
	_, err := e.led.Reserve(context.Background(), e.inst, 20) // 1510-20 < 1500
	deferred(t, err, ReasonBelowFloor)
}

func TestObserveIgnoresStaleOutOfOrderHeaders(t *testing.T) {
	e := newEnv(t)
	reset := t0.Add(30 * time.Minute)
	e.seed(t, 1700, reset, t0)
	// A late response from the same window reports a higher number: keep the lower.
	if err := e.led.Observe(context.Background(), e.inst, hdr(
		"X-RateLimit-Remaining", "1900", "X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))); err != nil {
		t.Fatal(err)
	}
	_, err := e.led.Reserve(context.Background(), e.inst, 250) // 1700-250 < 1500 but 1900-250 >= 1500
	deferred(t, err, ReasonBelowFloor)
}

func TestSecondaryLimitWithoutHeadersBacksOff60120240ThenGivesUp(t *testing.T) {
	e := newEnv(t)
	e.seed(t, 4000, t0.Add(30*time.Minute), t0)
	ctx := context.Background()
	for i, want := range []time.Duration{60 * time.Second, 120 * time.Second, 240 * time.Second} {
		out, err := e.led.RecordLimited(ctx, e.inst, http.Header{})
		if err != nil {
			t.Fatal(err)
		}
		if got := out.BackoffUntil.Sub(e.clock.Now()); got != want {
			t.Fatalf("strike %d backoff = %v, want %v", i+1, got, want)
		}
		if out.GiveUp {
			t.Fatalf("strike %d must not give up", i+1)
		}
	}
	out, err := e.led.RecordLimited(ctx, e.inst, http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	if !out.GiveUp {
		t.Fatal("strike 4 must give up (partial_reason=backoff)")
	}
	if got := out.BackoffUntil.Sub(e.clock.Now()); got > MaxBackoff {
		t.Fatalf("backoff %v exceeds 15 min", got)
	}
	// While in backoff every reservation defers to backoff_until.
	_, err = e.led.Reserve(ctx, e.inst, 1)
	d := deferred(t, err, ReasonBackoff)
	if d.DueAt.Before(out.BackoffUntil) {
		t.Fatalf("DueAt %v before backoff_until %v", d.DueAt, out.BackoffUntil)
	}
}

func TestSuccessfulObserveResetsSecondaryStrikes(t *testing.T) {
	e := newEnv(t)
	reset := t0.Add(30 * time.Minute)
	e.seed(t, 4000, reset, t0)
	ctx := context.Background()
	_, _ = e.led.RecordLimited(ctx, e.inst, http.Header{})
	_, _ = e.led.RecordLimited(ctx, e.inst, http.Header{})
	e.clock.Advance(3 * time.Minute)
	if err := e.led.Observe(ctx, e.inst, hdr(
		"X-RateLimit-Remaining", "3900", "X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))); err != nil {
		t.Fatal(err)
	}
	out, _ := e.led.RecordLimited(ctx, e.inst, http.Header{})
	if got := out.BackoffUntil.Sub(e.clock.Now()); got != 60*time.Second {
		t.Fatalf("backoff after success = %v, want 60s", got)
	}
}

func TestRetryAfterAndResetSetBackoffToTheLater(t *testing.T) {
	e := newEnv(t)
	reset := t0.Add(10 * time.Minute)
	e.seed(t, 0, reset, t0)
	ctx := context.Background()

	// Retry-After 120 s is earlier than reset (10 min): reset wins.
	out, err := e.led.RecordLimited(ctx, e.inst, hdr(
		"Retry-After", "120", "X-RateLimit-Remaining", "0", "X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10)))
	if err != nil {
		t.Fatal(err)
	}
	if !out.BackoffUntil.Equal(reset) {
		t.Fatalf("backoff = %v, want reset %v", out.BackoffUntil, reset)
	}
	// Retry-After 20 min is later than reset: Retry-After wins.
	out, err = e.led.RecordLimited(ctx, e.inst, hdr("Retry-After", "1200"))
	if err != nil {
		t.Fatal(err)
	}
	if want := t0.Add(20 * time.Minute); !out.BackoffUntil.Equal(want) {
		t.Fatalf("backoff = %v, want %v", out.BackoffUntil, want)
	}
}

func TestRecheckAtExecutionSeesNewBackoffAndFloor(t *testing.T) {
	e := newEnv(t)
	e.seed(t, 4000, t0.Add(30*time.Minute), t0)
	ctx := context.Background()
	r, err := e.led.Reserve(ctx, e.inst, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Recheck(ctx); err != nil {
		t.Fatalf("Recheck while healthy: %v", err)
	}
	if _, err := e.led.RecordLimited(ctx, e.inst, http.Header{}); err != nil {
		t.Fatal(err)
	}
	deferred(t, r.Recheck(ctx), ReasonBackoff)
}

func TestRecheckFailsForExpiredReservation(t *testing.T) {
	e := newEnv(t)
	e.seed(t, 4000, t0.Add(30*time.Minute), t0)
	ctx := context.Background()
	r, err := e.led.Reserve(ctx, e.inst, 10)
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(ReservationTTL + time.Second)
	if err := r.Recheck(ctx); err == nil {
		t.Fatal("expired reservation must not recheck clean")
	}
}

func rateHeaders(remaining int, reset time.Time) http.Header {
	h := http.Header{}
	h.Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	h.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
	return h
}

func (e *env) storedRemaining(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT remaining FROM gh_installation_budget WHERE installation_id=$1`, e.inst).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRefreshNeverOverwritesNewerLowerObservation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	reset := t0.Add(50 * time.Minute)
	e.fetch.snap = Snapshot{Remaining: 1600, ResetAt: reset}
	// Another call returns 1500 while GET /rate_limit (which read 1600) is in flight.
	e.fetch.during = func() {
		if err := e.led.Observe(ctx, e.inst, rateHeaders(1500, reset)); err != nil {
			t.Error(err)
		}
	}
	_, err := e.led.Reserve(ctx, e.inst, 10)
	deferred(t, err, ReasonBelowFloor)
	if got := e.storedRemaining(t); got != 1500 {
		t.Fatalf("remaining = %d, want 1500: refresh overwrote the lower observation", got)
	}
}

func (e *env) settledReservations(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM gh_budget_reservation WHERE installation_id=$1 AND substr(id::text,15,1)='0'`, e.inst).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCompleteWithoutHeadersKeepsTheCostOnTheLedger(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.seed(t, 1600, t0.Add(30*time.Minute), t0)

	first, err := e.led.Reserve(ctx, e.inst, 60)
	if err != nil {
		t.Fatal(err)
	}
	// The call timed out after GitHub received it: no headers.
	if err := first.Complete(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if e.settledReservations(t) != 1 {
		t.Fatal("header-less call must stay on the ledger as a settled reservation")
	}
	// Repeating the call must not reuse the old apparent budget: 1600-60-60 < 1500.
	_, err = e.led.Reserve(ctx, e.inst, 60)
	deferred(t, err, ReasonBelowFloor)
}

// A and B are in flight. GitHub processes A first (header 1550), then B
// (true count 1500). B completes without headers first, then A completes with
// its older header. The ledger must still account for B.
func TestOutOfOrderCompletionsDoNotLoseAHeaderlessCall(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	reset := t0.Add(30 * time.Minute)
	e.seed(t, 1600, reset, t0)
	a, err := e.led.Reserve(ctx, e.inst, 50)
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.led.Reserve(ctx, e.inst, 50)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Complete(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.Complete(ctx, rateHeaders(1550, reset)); err != nil {
		t.Fatal(err)
	}
	// True count is 1500: one more 50-cost call would cross the floor.
	_, err = e.led.Reserve(ctx, e.inst, 50)
	deferred(t, err, ReasonBelowFloor)
}

// B times out while still in transit and settles. A is reserved afterwards,
// reaches GitHub first and returns 1550. B can still arrive, leaving 1500, so
// A's response must not drop B's cost.
func TestDelayedTimeoutCostSurvivesLaterResponse(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	reset := t0.Add(30 * time.Minute)
	e.seed(t, 1600, reset, t0)
	b, err := e.led.Reserve(ctx, e.inst, 50)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Complete(ctx, nil); err != nil {
		t.Fatal(err)
	}
	a, err := e.led.Reserve(ctx, e.inst, 50)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Complete(ctx, rateHeaders(1550, reset)); err != nil {
		t.Fatal(err)
	}
	if e.settledReservations(t) != 1 {
		t.Fatal("a normal response must not clear an uncertain cost")
	}
	// B may still land: 1550 - 50 (B) - 50 < 1500.
	_, err = e.led.Reserve(ctx, e.inst, 50)
	deferred(t, err, ReasonBelowFloor)
}

func (e *env) ageSettled(t *testing.T, by time.Duration) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE gh_budget_reservation SET created_at = created_at - make_interval(secs => $2)
		  WHERE installation_id=$1 AND substr(id::text,15,1)='0'`, e.inst, by.Seconds()); err != nil {
		t.Fatal(err)
	}
}

func (e *env) settleHeaderless(t *testing.T, cost int) {
	t.Helper()
	g, err := e.led.Reserve(context.Background(), e.inst, cost)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Complete(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshStartedTooSoonKeepsUncertainCost(t *testing.T) {
	e := newEnv(t)
	e.seed(t, 1600, t0.Add(50*time.Minute), t0)
	e.settleHeaderless(t, 50)
	e.clock.Advance(StaleAfter + time.Second)
	e.fetch.snap = Snapshot{Remaining: 1650, ResetAt: t0.Add(50 * time.Minute)}
	if _, err := e.led.Reserve(context.Background(), e.inst, 10); err != nil {
		t.Fatal(err)
	}
	if e.settledReservations(t) != 1 {
		t.Fatal("a refresh that started before the call could have landed must keep its cost")
	}
}

func TestRefreshAfterClearDelayDropsUncertainCost(t *testing.T) {
	e := newEnv(t)
	e.seed(t, 1600, t0.Add(50*time.Minute), t0)
	e.settleHeaderless(t, 50)
	e.ageSettled(t, UncertainClearAfter+time.Second)
	e.clock.Advance(StaleAfter + time.Second)
	e.fetch.snap = Snapshot{Remaining: 1550, ResetAt: t0.Add(50 * time.Minute)}
	if _, err := e.led.Reserve(context.Background(), e.inst, 10); err != nil {
		t.Fatal(err)
	}
	if e.settledReservations(t) != 0 {
		t.Fatal("GET /rate_limit started after the clear delay already counts the call")
	}
}

func TestCompleteWithHeadersMergesInsteadOfDebiting(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	reset := t0.Add(30 * time.Minute)
	e.seed(t, 1600, reset, t0)
	r, err := e.led.Reserve(ctx, e.inst, 60)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Complete(ctx, rateHeaders(1590, reset)); err != nil {
		t.Fatal(err)
	}
	if got := e.storedRemaining(t); got != 1590 {
		t.Fatalf("remaining = %d, want 1590 from the headers", got)
	}
	if e.activeReservations(t) != 0 {
		t.Fatal("reservation not deleted on complete")
	}
}
