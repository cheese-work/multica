package mint

import (
	"context"
	"net/http"
	"time"
)

// Resource is one /rate_limit resource of the CI App installation.
type Resource struct {
	Remaining int
	Reset     time.Time
}

// Reading is one /rate_limit observation.
type Reading struct {
	Core    Resource
	GraphQL Resource
}

// RateLimiter reads GET /rate_limit for the CI App installation.
type RateLimiter interface {
	RateLimit(ctx context.Context) (Reading, error)
}

// Cutoff and cost constants (rule 6).
const (
	QuotaFloor    = 500
	IssueCost     = 100
	ReadingMaxAge = 30 * time.Second
)

// Quota is the best-effort issuance cutoff. It is not a reserve: a token
// issued earlier can still spend after the check. Callers must serialize Admit
// (the broker's mutex does).
type Quota struct {
	src       RateLimiter
	now       func() time.Time
	have      bool
	reading   Reading
	fetchedAt time.Time
}

// NewQuota builds a Quota on a rate limit source and a clock.
func NewQuota(src RateLimiter, now func() time.Time) *Quota {
	return &Quota{src: src, now: now}
}

func (q *Quota) fresh(now time.Time) bool {
	return q.have && now.Sub(q.fetchedAt) <= ReadingMaxAge &&
		now.Before(q.reading.Core.Reset) && now.Before(q.reading.GraphQL.Reset)
}

func usable(r Reading, now time.Time) bool {
	return r.Core.Remaining >= 0 && r.GraphQL.Remaining >= 0 &&
		now.Before(r.Core.Reset) && now.Before(r.GraphQL.Reset)
}

// Admit returns nil, or a *Refusal: 429 below the floor, 503 without a usable
// reading. A reading is used for at most ReadingMaxAge and until its reset.
// One refresh is tried; a reading it does not fix is never served from cache.
// Each admission subtracts IssueCost from the cached core.remaining.
func (q *Quota) Admit(ctx context.Context) error {
	now := q.now()
	if !q.fresh(now) {
		q.have = false
		start := now
		r, err := q.src.RateLimit(ctx)
		// Judge the reading by the clock after the call: a slow call can cross a
		// reset, and a reading is as old as the moment the call began.
		now = q.now()
		if err != nil || now.Sub(start) > ReadingMaxAge || !usable(r, now) {
			return &Refusal{Status: http.StatusServiceUnavailable, Reason: "quota_unavailable", RetryAfter: 30 * time.Second}
		}
		q.reading, q.fetchedAt, q.have = r, start, true
	}
	if q.reading.Core.Remaining < QuotaFloor {
		return &Refusal{Status: http.StatusTooManyRequests, Reason: "quota_core", RetryAfter: q.reading.Core.Reset.Sub(now)}
	}
	if q.reading.GraphQL.Remaining < QuotaFloor {
		return &Refusal{Status: http.StatusTooManyRequests, Reason: "quota_graphql", RetryAfter: q.reading.GraphQL.Reset.Sub(now)}
	}
	q.reading.Core.Remaining -= IssueCost
	return nil
}
