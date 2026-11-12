package mint

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func newQuota(t *testing.T, lim *fakeLimiter) (*Quota, *fakeClock) {
	t.Helper()
	clk := newClock()
	return NewQuota(lim, clk.Now), clk
}

func wantRefusal(t *testing.T, err error, status int, reason string) *Refusal {
	t.Helper()
	var ref *Refusal
	if !errors.As(err, &ref) {
		t.Fatalf("err = %v, want *Refusal %d %s", err, status, reason)
	}
	if ref.Status != status || ref.Reason != reason {
		t.Fatalf("refusal = %d %s, want %d %s", ref.Status, ref.Reason, status, reason)
	}
	return ref
}

func TestQuotaAdmitsAboveFloorAndSubtractsCostPerIssue(t *testing.T) {
	lim := &fakeLimiter{}
	q, clk := newQuota(t, lim)
	lim.set(reading(clk.Now(), 700, 5000), nil)
	ctx := context.Background()
	// 700 -> 600 -> 500 -> 400: the first three pass (>= 500), the fourth is below the floor.
	for i := 0; i < 3; i++ {
		if err := q.Admit(ctx); err != nil {
			t.Fatalf("admit %d: %v", i, err)
		}
	}
	wantRefusal(t, q.Admit(ctx), http.StatusTooManyRequests, "quota_core")
	if got := lim.calls.Load(); got != 1 {
		t.Fatalf("rate_limit calls = %d, want 1 (cached reading reused)", got)
	}
}

func TestQuotaCutoffCoreAndGraphQLSeparately(t *testing.T) {
	for _, c := range []struct {
		name          string
		core, graphql int
		reason        string
	}{
		{"core low", 499, 5000, "quota_core"},
		{"graphql low", 5000, 499, "quota_graphql"},
		{"core at floor passes", 500, 5000, ""},
		{"graphql at floor passes", 5000, 500, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			lim := &fakeLimiter{}
			q, clk := newQuota(t, lim)
			lim.set(reading(clk.Now(), c.core, c.graphql), nil)
			err := q.Admit(context.Background())
			if c.reason == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			ref := wantRefusal(t, err, http.StatusTooManyRequests, c.reason)
			if ref.RetryAfter <= 0 {
				t.Fatalf("RetryAfter = %v, want > 0 (time to reset)", ref.RetryAfter)
			}
		})
	}
}

func TestQuotaFailedReadingIs503(t *testing.T) {
	lim := &fakeLimiter{}
	q, _ := newQuota(t, lim)
	lim.set(Reading{}, errors.New("github down"))
	wantRefusal(t, q.Admit(context.Background()), http.StatusServiceUnavailable, "quota_unavailable")
}

func TestQuotaStaleReadingRefreshedOnce(t *testing.T) {
	lim := &fakeLimiter{}
	q, clk := newQuota(t, lim)
	lim.set(reading(clk.Now(), 5000, 5000), nil)
	ctx := context.Background()
	if err := q.Admit(ctx); err != nil {
		t.Fatal(err)
	}
	clk.Advance(ReadingMaxAge - time.Second)
	if err := q.Admit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := lim.calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1 inside max age", got)
	}
	clk.Advance(2 * time.Second)
	if err := q.Admit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := lim.calls.Load(); got != 2 {
		t.Fatalf("calls = %d, want 2 after max age", got)
	}
}

func TestQuotaStaleReadingWhenRefreshFailsIs503NotServedFromCache(t *testing.T) {
	lim := &fakeLimiter{}
	q, clk := newQuota(t, lim)
	lim.set(reading(clk.Now(), 5000, 5000), nil)
	ctx := context.Background()
	if err := q.Admit(ctx); err != nil {
		t.Fatal(err)
	}
	clk.Advance(ReadingMaxAge + time.Second)
	lim.set(Reading{}, errors.New("github down"))
	wantRefusal(t, q.Admit(ctx), http.StatusServiceUnavailable, "quota_unavailable")
	if got := lim.calls.Load(); got != 2 {
		t.Fatalf("calls = %d, want exactly one refresh attempt", got)
	}
}

func TestQuotaRefreshAfterResetRollover(t *testing.T) {
	lim := &fakeLimiter{}
	q, clk := newQuota(t, lim)
	ctx := context.Background()
	lim.set(Reading{
		Core:    Resource{Remaining: 400, Reset: clk.Now().Add(10 * time.Second)},
		GraphQL: Resource{Remaining: 5000, Reset: clk.Now().Add(10 * time.Second)},
	}, nil)
	wantRefusal(t, q.Admit(ctx), http.StatusTooManyRequests, "quota_core")
	// The window rolls over before the cache is 30 s old: Admit must refresh.
	clk.Advance(11 * time.Second)
	lim.set(Reading{
		Core:    Resource{Remaining: 5000, Reset: clk.Now().Add(time.Hour)},
		GraphQL: Resource{Remaining: 5000, Reset: clk.Now().Add(time.Hour)},
	}, nil)
	if err := q.Admit(ctx); err != nil {
		t.Fatalf("after rollover: %v", err)
	}
	if got := lim.calls.Load(); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}
}

func TestQuotaRefreshThatStillShowsPastResetIs503(t *testing.T) {
	lim := &fakeLimiter{}
	q, clk := newQuota(t, lim)
	lim.set(Reading{
		Core:    Resource{Remaining: 5000, Reset: clk.Now().Add(-time.Minute)},
		GraphQL: Resource{Remaining: 5000, Reset: clk.Now().Add(time.Hour)},
	}, nil)
	wantRefusal(t, q.Admit(context.Background()), http.StatusServiceUnavailable, "quota_unavailable")
}

func TestQuotaRejectsNegativeRemaining(t *testing.T) {
	lim := &fakeLimiter{}
	q, clk := newQuota(t, lim)
	lim.set(reading(clk.Now(), -1, 5000), nil)
	wantRefusal(t, q.Admit(context.Background()), http.StatusServiceUnavailable, "quota_unavailable")
}

func TestQuotaSlowRefreshCrossingResetIs503(t *testing.T) {
	lim := &fakeLimiter{}
	q, clk := newQuota(t, lim)
	lim.set(Reading{
		Core:    Resource{Remaining: 5000, Reset: clk.Now().Add(5 * time.Second)},
		GraphQL: Resource{Remaining: 5000, Reset: clk.Now().Add(time.Hour)},
	}, nil)
	lim.onCall = func() { clk.Advance(6 * time.Second) } // the reset passes during the call
	wantRefusal(t, q.Admit(context.Background()), http.StatusServiceUnavailable, "quota_unavailable")
}

func TestQuotaCallOver30sIs503(t *testing.T) {
	lim := &fakeLimiter{}
	q, clk := newQuota(t, lim)
	lim.set(reading(clk.Now(), 5000, 5000), nil)
	lim.onCall = func() { clk.Advance(ReadingMaxAge + time.Second) }
	wantRefusal(t, q.Admit(context.Background()), http.StatusServiceUnavailable, "quota_unavailable")
}
