package ciwatch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/ghbudget"
)

// These tests need a migrated database: DATABASE_URL=... go test ./internal/ciwatch -run Ledger

var ledgerT0 = time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)

// ledgerWindow is the reset of the seeded rate-limit window.
var ledgerWindow = ledgerT0.Add(50 * time.Minute)

type ledgerClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *ledgerClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *ledgerClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type fullFetcher struct{}

func (fullFetcher) RateLimit(context.Context, int64) (ghbudget.Snapshot, error) {
	return ghbudget.Snapshot{Remaining: 5000, ResetAt: ledgerWindow}, nil
}

var ledgerInstSeq atomic.Int64

// ledgerEnv is one installation with a healthy 5000-call budget.
func ledgerEnv(t *testing.T) (LedgerBudget, *pgxpool.Pool, *ledgerClock, int64) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	inst := 7_000_000_000 + time.Now().UnixNano()%1_000_000*100 + ledgerInstSeq.Add(1)
	t.Cleanup(func() {
		c := context.Background()
		if p, err := pgxpool.New(c, url); err == nil {
			p.Exec(c, `DELETE FROM gh_budget_reservation WHERE installation_id = $1`, inst)
			p.Exec(c, `DELETE FROM gh_installation_budget WHERE installation_id = $1`, inst)
			p.Close()
		}
		pool.Close()
	})
	if _, err := pool.Exec(ctx, `INSERT INTO gh_installation_budget (installation_id, remaining, reset_at, observed_at) VALUES ($1, 5000, $2, $3)`,
		inst, ledgerWindow, ledgerT0); err != nil {
		t.Fatal(err)
	}
	clk := &ledgerClock{t: ledgerT0}
	led := ghbudget.New(pool, fullFetcher{}, ghbudget.WithClock(clk.Now), ghbudget.WithJitter(func() time.Duration { return 0 }))
	return LedgerBudget{L: led}, pool, clk, inst
}

func rateHeaders(remaining int, reset time.Time) http.Header {
	h := http.Header{}
	h.Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	h.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
	return h
}

// Another consumer spent the installation budget down below Floor. The response
// headers say so; the watcher must stop instead of spending its reservation.
func TestLedgerBudgetReleaseSharesResponseHeaders(t *testing.T) {
	b, _, _, inst := ledgerEnv(t)
	ctx := context.Background()
	release, err := b.Reserve(ctx, inst, 1)
	if err != nil {
		t.Fatal(err)
	}
	release(rateHeaders(ghbudget.Floor-100, ledgerWindow))
	if _, err := b.Reserve(ctx, inst, 1); !errors.Is(err, ErrNoBudget) {
		t.Fatalf("Reserve after the headers showed %d left: err=%v, want ErrNoBudget", ghbudget.Floor-100, err)
	}
}

// The same, end to end: the jobs-list response shows the shared budget under
// Floor, so the log download is never attempted.
func TestLedgerBudgetStopsCollectorWhenSharedBudgetDrops(t *testing.T) {
	b, _, _, inst := ledgerEnv(t)
	gh := &fakeGH{
		jobsByPage: [][]Job{{failedJob(1, "build", t0)}},
		logs:       map[int64]func(context.Context) io.Reader{1: text("API rate limit exceeded\n")},
		hdr:        rateHeaders(ghbudget.Floor-100, ledgerWindow),
	}
	c := &Collector{GH: gh, Budget: b, Runs: fakeRuns{}}
	in := baseInput()
	in.Run.InstallationID = inst
	res, err := c.Handle(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(gh.logCalls) != 0 || res.Event.Evidence != EvidencePartial || res.Event.PartialReason != PartialBudget {
		t.Fatalf("logCalls=%v evidence=%s reason=%s, want no log call and partial/budget",
			gh.logCalls, res.Event.Evidence, res.Event.PartialReason)
	}
}

func limitedHeader(retryAfter time.Duration, remaining int, reset time.Time) http.Header {
	h := rateHeaders(remaining, reset)
	if retryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
	}
	return h
}

// GitHub's reset is 20 minutes away. The old code kept only a 5-minute wait, so
// the watcher resumed 15 minutes early.
func TestLedgerBudgetLimitedHonorsResetBeyondFiveMinutes(t *testing.T) {
	b, _, clk, inst := ledgerEnv(t)
	ctx := context.Background()
	e := &RateLimitError{RetryAfter: 5 * time.Minute, Header: limitedHeader(0, 0, ledgerT0.Add(20*time.Minute))}
	if err := b.Limited(ctx, inst, e); err != nil {
		t.Fatal(err)
	}
	clk.Advance(6 * time.Minute)
	if _, err := b.Reserve(ctx, inst, 1); !errors.Is(err, ErrBackoff) {
		t.Fatalf("6 min after a limit that resets in 20: err=%v, want ErrBackoff", err)
	}
	clk.Advance(15 * time.Minute) // 21 min: past the reset
	if _, err := b.Reserve(ctx, inst, 1); err != nil {
		t.Fatalf("after the reset passed: err=%v, want a reservation", err)
	}
}

// Retry-After and reset both count; the later one wins.
func TestLedgerBudgetLimitedUsesLaterOfResetAndRetryAfter(t *testing.T) {
	b, _, clk, inst := ledgerEnv(t)
	ctx := context.Background()
	e := &RateLimitError{RetryAfter: 5 * time.Minute, Header: limitedHeader(15*time.Minute, 0, ledgerT0.Add(2*time.Minute))}
	if err := b.Limited(ctx, inst, e); err != nil {
		t.Fatal(err)
	}
	clk.Advance(10 * time.Minute)
	if _, err := b.Reserve(ctx, inst, 1); !errors.Is(err, ErrBackoff) {
		t.Fatalf("10 min after Retry-After 15 min: err=%v, want ErrBackoff", err)
	}
	clk.Advance(6 * time.Minute)
	if _, err := b.Reserve(ctx, inst, 1); err != nil {
		t.Fatalf("after Retry-After passed: err=%v, want a reservation", err)
	}
}

// Without headers only the client's wait is known; it still applies.
func TestLedgerBudgetLimitedWithoutHeadersUsesRetryAfter(t *testing.T) {
	b, _, clk, inst := ledgerEnv(t)
	ctx := context.Background()
	if err := b.Limited(ctx, inst, &RateLimitError{RetryAfter: 4 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	clk.Advance(3 * time.Minute)
	if _, err := b.Reserve(ctx, inst, 1); !errors.Is(err, ErrBackoff) {
		t.Fatalf("3 min into a 4-min wait: err=%v, want ErrBackoff", err)
	}
}

// A backoff that was not stored must not look stored.
func TestLedgerBudgetLimitedReturnsWriteError(t *testing.T) {
	b, pool, _, inst := ledgerEnv(t)
	pool.Close() // every ledger write now fails
	err := b.Limited(context.Background(), inst, &RateLimitError{RetryAfter: time.Minute, Header: limitedHeader(0, 0, ledgerT0.Add(20*time.Minute))})
	if err == nil {
		t.Fatal("Limited returned nil although the ledger write failed")
	}
}
