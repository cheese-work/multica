package mint

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

var t0 = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: t0} }

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeLimiter is a fake /rate_limit.
type fakeLimiter struct {
	mu      sync.Mutex
	calls   atomic.Int32
	reading Reading
	err     error
	onCall  func() // runs inside RateLimit, before it returns (a slow call)
}

func (f *fakeLimiter) RateLimit(context.Context) (Reading, error) {
	f.calls.Add(1)
	if f.onCall != nil {
		f.onCall()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reading, f.err
}

func (f *fakeLimiter) set(r Reading, err error) {
	f.mu.Lock()
	f.reading, f.err = r, err
	f.mu.Unlock()
}

func reading(now time.Time, core, graphql int) Reading {
	reset := now.Add(40 * time.Minute)
	return Reading{Core: Resource{core, reset}, GraphQL: Resource{graphql, reset}}
}

// fakeMinter records mint and revoke calls and the highest concurrency seen.
type fakeMinter struct {
	mu        sync.Mutex
	minted    []mintCall
	revoked   []string
	inflight  atomic.Int32
	maxSeen   atomic.Int32
	err       error
	revokeErr error
	seq       int
	hold      time.Duration
}

type mintCall struct {
	repoID int64
	perms  Permissions
}

func (m *fakeMinter) Mint(_ context.Context, repoID int64, perms Permissions) (Token, error) {
	n := m.inflight.Add(1)
	defer m.inflight.Add(-1)
	for {
		old := m.maxSeen.Load()
		if n <= old || m.maxSeen.CompareAndSwap(old, n) {
			break
		}
	}
	time.Sleep(m.hold)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return Token{}, m.err
	}
	m.seq++
	m.minted = append(m.minted, mintCall{repoID, perms})
	return Token{
		Value:       Secret(fmt.Sprintf("ghs_SECRETTOKEN%04d", m.seq)),
		ExpiresAt:   t0.Add(time.Hour),
		Permissions: perms,
	}, nil
}

func (m *fakeMinter) Revoke(_ context.Context, token Secret) error {
	m.mu.Lock()
	m.revoked = append(m.revoked, token.Reveal())
	m.mu.Unlock()
	return m.revokeErr
}

type memAudit struct {
	mu   sync.Mutex
	rows []Row
	err  error
}

func (a *memAudit) Record(_ context.Context, row Row) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return a.err
	}
	a.rows = append(a.rows, row)
	return nil
}

func (a *memAudit) all() []Row {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Row(nil), a.rows...)
}

var errBoom = errors.New("boom")
