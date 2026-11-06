// Package ghbudget is the shared GitHub API budget ledger of the CI failure
// watcher (CHE-4275, S3 of CHE-4265). One ledger row per App installation is
// shared by every repo and workspace on it (tables gh_installation_budget and
// gh_budget_reservation, migration 986).
//
// A watcher call first reserves its estimated cost. The reservation is refused
// when it would leave less than Floor requests. Refusal never cancels work: it
// returns a DeferredError whose DueAt is when to ask again.
//
// The strike counter and the 60 s refresh gap live in process memory; the
// ledger, the backoff and the reservations are shared through the database.
package ghbudget

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/singleflight"
)

const (
	// Floor is the request count the watcher never spends below.
	Floor = 1500
	// StaleAfter is the age after which the ledger is refreshed from GET /rate_limit.
	StaleAfter = 5 * time.Minute
	// MinRefreshGap is the least time between two refresh attempts per installation.
	MinRefreshGap = 60 * time.Second
	// ReservationTTL bounds how long a forgotten reservation counts.
	ReservationTTL = 2 * time.Minute
	// UncertainClearAfter is how long after a header-less call settled a
	// refresh must have started before it counts that call: the 20 s HTTP
	// client timeout plus a 60 s margin. By then the call has reached GitHub or
	// been dropped.
	UncertainClearAfter = 80 * time.Second
	// MaxJitter spreads deferred calls so they do not wake together.
	MaxJitter = 60 * time.Second
	// MaxBackoff caps the header-less secondary-limit backoff.
	MaxBackoff = 15 * time.Minute
	// MaxStrikes is how many header-less limits back off before the caller
	// must give up on the event (partial_reason=backoff).
	MaxStrikes   = 3
	firstBackoff = 60 * time.Second
)

// Snapshot is the authoritative budget of one installation.
type Snapshot struct {
	Remaining int
	ResetAt   time.Time
}

// Fetcher reads GET /rate_limit with the installation token. That endpoint
// does not count against the REST rate limit.
type Fetcher interface {
	RateLimit(ctx context.Context, installationID int64) (Snapshot, error)
}

// Reason says why a reservation was deferred.
type Reason string

const (
	ReasonUnknown    Reason = "unknown_budget"
	ReasonBelowFloor Reason = "below_floor"
	ReasonBackoff    Reason = "backoff"
)

// DeferredError is the "not now" answer. DueAt is the new due time (reset or
// backoff end plus jitter); the caller moves its action there, never cancels.
type DeferredError struct {
	Reason Reason
	DueAt  time.Time
}

func (e *DeferredError) Error() string {
	return fmt.Sprintf("github budget deferred (%s) until %s", e.Reason, e.DueAt.Format(time.RFC3339))
}

// ErrReservationExpired means the reservation lapsed before execution; reserve again.
var ErrReservationExpired = errors.New("github budget reservation expired")

var errNeedsRefresh = errors.New("ledger needs refresh")

// Outcome is what RecordLimited decided.
type Outcome struct {
	BackoffUntil time.Time
	// GiveUp: header-less limits exceeded MaxStrikes. Stop calling for this event.
	GiveUp bool
}

// Ledger is safe for concurrent use.
type Ledger struct {
	pool   *pgxpool.Pool
	fetch  Fetcher
	now    func() time.Time
	jitter func() time.Duration

	sf          singleflight.Group
	mu          sync.Mutex
	lastAttempt map[int64]time.Time
	strikes     map[int64]int
}

type Option func(*Ledger)

func WithClock(now func() time.Time) Option    { return func(l *Ledger) { l.now = now } }
func WithJitter(j func() time.Duration) Option { return func(l *Ledger) { l.jitter = j } }

func New(pool *pgxpool.Pool, fetch Fetcher, opts ...Option) *Ledger {
	l := &Ledger{
		pool:        pool,
		fetch:       fetch,
		now:         time.Now,
		jitter:      func() time.Duration { return rand.N(MaxJitter + 1) },
		lastAttempt: map[int64]time.Time{},
		strikes:     map[int64]int{},
	}
	for _, o := range opts {
		o(l)
	}
	return l
}

// Reservation is a claim on part of the installation's budget.
type Reservation struct {
	l    *Ledger
	inst int64
	id   string
	cost int
}

// Reserve claims cost requests, refreshing the ledger first when it is empty,
// stale (> 5 min) or past its reset. A *DeferredError means: do not call now.
func (l *Ledger) Reserve(ctx context.Context, inst int64, cost int) (*Reservation, error) {
	if cost <= 0 {
		return nil, fmt.Errorf("ghbudget: cost must be positive, got %d", cost)
	}
	for attempt := 0; attempt < 2; attempt++ {
		r, err := l.tryReserve(ctx, inst, cost)
		if !errors.Is(err, errNeedsRefresh) {
			return r, err
		}
		if rerr := l.refresh(ctx, inst); rerr != nil {
			return nil, l.unknown()
		}
	}
	return nil, l.unknown()
}

func (l *Ledger) unknown() *DeferredError {
	return &DeferredError{Reason: ReasonUnknown, DueAt: l.now().Add(MinRefreshGap + l.jitter())}
}

func (l *Ledger) tryReserve(ctx context.Context, inst int64, cost int) (*Reservation, error) {
	var res *Reservation
	err := l.inLockedTx(ctx, inst, func(tx pgx.Tx) error {
		now := l.now()
		if err := l.evaluate(ctx, tx, inst, cost, "", now); err != nil {
			return err
		}
		var id string
		if err := tx.QueryRow(ctx,
			`INSERT INTO gh_budget_reservation (installation_id, cost, expires_at, created_at)
			 VALUES ($1,$2,$3, clock_timestamp()) RETURNING id::text`,
			inst, cost, now.Add(ReservationTTL)).Scan(&id); err != nil {
			return err
		}
		res = &Reservation{l: l, inst: inst, id: id, cost: cost}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// Recheck is the check at execution time: still reserved, not in backoff, and
// the floor still holds with every other active reservation counted.
func (r *Reservation) Recheck(ctx context.Context) error {
	return r.l.inLockedTx(ctx, r.inst, func(tx pgx.Tx) error {
		now := r.l.now()
		var live bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM gh_budget_reservation WHERE id=$1::uuid AND expires_at > $2)`,
			r.id, now).Scan(&live); err != nil {
			return err
		}
		if !live {
			return ErrReservationExpired
		}
		err := r.l.evaluate(ctx, tx, r.inst, r.cost, r.id, now)
		if errors.Is(err, errNeedsRefresh) {
			return r.l.unknown()
		}
		return err
	})
}

// Complete settles a reservation after the call returned. Pass the response
// headers, or nil when the call failed or timed out.
//
// Usable headers are merged into the ledger (the lower count wins within a
// window). A call without usable headers may still reach GitHub later, so its
// reservation stays on the ledger as an uncertain cost. No response clears an
// uncertain cost, because responses arrive out of order. Only a refresh that
// started UncertainClearAfter after the settlement, or the window reset,
// clears it. Over-reserving only defers a call; under-reserving breaks the
// floor.
func (r *Reservation) Complete(ctx context.Context, h http.Header) error {
	return r.l.inLockedTx(ctx, r.inst, func(tx pgx.Tx) error {
		remaining, reset, ok := parseRateHeaders(h)
		if !ok {
			return r.settle(ctx, tx)
		}
		if _, err := r.l.upsertObservation(ctx, tx, r.inst, remaining, reset); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM gh_budget_reservation WHERE id=$1::uuid`, r.id)
		return err
	})
}

// settle turns the reservation into an uncertain cost that lasts until the
// window resets, and stamps created_at with the settle time. A settled call is
// marked by version nibble 0 in its id: gen_random_uuid() always writes 4
// there, so no live reservation looks settled. This needs no migration (the
// runner sorts file names as text, so a 1000_ file would run before 986_).
// If cleanup already removed the row, it is written again.
func (r *Reservation) settle(ctx context.Context, tx pgx.Tx) error {
	until := r.l.now().Add(ReservationTTL)
	tag, err := tx.Exec(ctx,
		`UPDATE gh_budget_reservation
		    SET id = overlay(id::text placing '0' from 15 for 1)::uuid,
		        created_at = clock_timestamp(),
		        expires_at = GREATEST(expires_at, COALESCE((SELECT reset_at FROM gh_installation_budget WHERE installation_id=$2), $3))
		  WHERE id=$1::uuid`, r.id, r.inst, until)
	if err != nil || tag.RowsAffected() > 0 {
		return err
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO gh_budget_reservation (id, installation_id, cost, expires_at, created_at)
		 VALUES (overlay($1::text placing '0' from 15 for 1)::uuid, $2, $3,
		         COALESCE((SELECT reset_at FROM gh_installation_budget WHERE installation_id=$2), $4), clock_timestamp())
		 ON CONFLICT (id) DO NOTHING`,
		r.id, r.inst, r.cost, until)
	return err
}

// Release deletes the reservation of a call that was never sent. A sent call
// must use Complete so its cost is accounted. Safe to repeat.
func (r *Reservation) Release(ctx context.Context) error {
	_, err := r.l.pool.Exec(ctx, `DELETE FROM gh_budget_reservation WHERE id=$1::uuid`, r.id)
	return err
}

// evaluate decides under the installation lock. exclude is the caller's own
// reservation id (Recheck) so it is not counted twice.
func (l *Ledger) evaluate(ctx context.Context, tx pgx.Tx, inst int64, cost int, exclude string, now time.Time) error {
	var (
		remaining int
		reset     *time.Time
		observed  time.Time
		backoff   *time.Time
	)
	err := tx.QueryRow(ctx,
		`SELECT remaining, reset_at, observed_at, backoff_until FROM gh_installation_budget WHERE installation_id=$1`,
		inst).Scan(&remaining, &reset, &observed, &backoff)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNeedsRefresh
	}
	if err != nil {
		return err
	}
	if backoff != nil && backoff.After(now) {
		return &DeferredError{Reason: ReasonBackoff, DueAt: backoff.Add(l.jitter())}
	}
	if reset == nil || now.After(*reset) || now.Sub(observed) > StaleAfter {
		return errNeedsRefresh
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM gh_budget_reservation WHERE installation_id=$1 AND expires_at <= $2`, inst, now); err != nil {
		return err
	}
	var held int
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(sum(cost),0)::int FROM gh_budget_reservation
		  WHERE installation_id=$1 AND expires_at > $2 AND ($3 = '' OR id::text <> $3)`,
		inst, now, exclude).Scan(&held); err != nil {
		return err
	}
	if remaining-held-cost < Floor {
		return &DeferredError{Reason: ReasonBelowFloor, DueAt: reset.Add(l.jitter())}
	}
	return nil
}

func (l *Ledger) inLockedTx(ctx context.Context, inst int64, fn func(pgx.Tx) error) error {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"ghbudget:reserve:"+strconv.FormatInt(inst, 10)); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// refresh runs GET /rate_limit once per installation at a time (in this
// process by single flight, across processes by an advisory lock), at most
// once per MinRefreshGap. A failed refresh leaves the ledger unchanged.
func (l *Ledger) refresh(ctx context.Context, inst int64) error {
	_, err, _ := l.sf.Do(strconv.FormatInt(inst, 10), func() (any, error) {
		return nil, l.refreshLocked(ctx, inst)
	})
	return err
}

func (l *Ledger) refreshLocked(ctx context.Context, inst int64) error {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	key := "ghbudget:refresh:" + strconv.FormatInt(inst, 10)
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, key); err != nil {
		return err
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, key)
	}()

	now := l.now()
	// Another caller may have refreshed while this one waited for the lock.
	var (
		reset    *time.Time
		observed time.Time
	)
	err = conn.QueryRow(ctx, `SELECT reset_at, observed_at FROM gh_installation_budget WHERE installation_id=$1`, inst).
		Scan(&reset, &observed)
	if err == nil && reset != nil && !now.After(*reset) && now.Sub(observed) <= StaleAfter {
		return nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}

	l.mu.Lock()
	last, tried := l.lastAttempt[inst]
	if tried && now.Sub(last) < MinRefreshGap {
		l.mu.Unlock()
		return errors.New("ghbudget: refresh attempted less than 60s ago")
	}
	l.lastAttempt[inst] = now
	l.mu.Unlock()

	var fetchStart time.Time
	if err := conn.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&fetchStart); err != nil {
		return err
	}
	snap, err := l.fetch.RateLimit(ctx, inst)
	if err != nil {
		return err
	}
	// A header observation may have landed while RateLimit was in flight, so
	// merge conservatively instead of overwriting.
	applied, err := l.upsertObservation(ctx, conn, inst, snap.Remaining, snap.ResetAt)
	if err != nil || !applied {
		return err
	}
	// GET /rate_limit counts an uncertain call once that call has either
	// reached GitHub or been dropped, which UncertainClearAfter guarantees.
	_, err = conn.Exec(ctx,
		`DELETE FROM gh_budget_reservation WHERE installation_id=$1 AND substr(id::text,15,1)='0' AND created_at < $2`,
		inst, fetchStart.Add(-UncertainClearAfter))
	return err
}

// Observe folds the rate-limit headers of a successful watcher response into
// the ledger and clears the secondary-limit strikes.
func (l *Ledger) Observe(ctx context.Context, inst int64, h http.Header) error {
	l.mu.Lock()
	delete(l.strikes, inst)
	l.mu.Unlock()
	return l.observeHeaders(ctx, inst, h)
}

func (l *Ledger) observeHeaders(ctx context.Context, inst int64, h http.Header) error {
	remaining, reset, ok := parseRateHeaders(h)
	if !ok {
		return nil
	}
	_, err := l.upsertObservation(ctx, l.pool, inst, remaining, reset)
	return err
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// upsertObservation merges one reading into the ledger without ever raising
// the count: same window keeps the lower count, an older window is ignored,
// a newer window replaces.
func (l *Ledger) upsertObservation(ctx context.Context, db execer, inst int64, remaining int, reset time.Time) (applied bool, err error) {
	tag, err := db.Exec(ctx,
		`INSERT INTO gh_installation_budget AS b (installation_id, remaining, reset_at, observed_at)
		 VALUES ($1,$2,$3,$4)
		 ON CONFLICT (installation_id) DO UPDATE
		   SET remaining = CASE WHEN b.reset_at IS NULL OR EXCLUDED.reset_at > b.reset_at
		                        THEN EXCLUDED.remaining ELSE LEAST(b.remaining, EXCLUDED.remaining) END,
		       reset_at = EXCLUDED.reset_at,
		       observed_at = EXCLUDED.observed_at
		   WHERE b.reset_at IS NULL OR EXCLUDED.reset_at >= b.reset_at`,
		inst, remaining, reset, l.now())
	return tag.RowsAffected() > 0, err
}

func parseRateHeaders(h http.Header) (remaining int, reset time.Time, ok bool) {
	rem, err1 := strconv.Atoi(strings.TrimSpace(h.Get("X-RateLimit-Remaining")))
	unix, err2 := strconv.ParseInt(strings.TrimSpace(h.Get("X-RateLimit-Reset")), 10, 64)
	if err1 != nil || err2 != nil || rem < 0 {
		return 0, time.Time{}, false
	}
	return rem, time.Unix(unix, 0).UTC(), true
}

// RecordLimited handles a 403/429 the caller classified as a rate limit. It
// sets the shared backoff_until to the later of the header reset (when the
// count is 0) and Retry-After. With neither header it backs off 60, 120, 240 s
// (capped at 15 min) and sets GiveUp after MaxStrikes such limits in a row.
func (l *Ledger) RecordLimited(ctx context.Context, inst int64, h http.Header) (Outcome, error) {
	now := l.now()
	var until time.Time
	if secs, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After"))); err == nil && secs >= 0 {
		until = now.Add(time.Duration(secs) * time.Second)
	}
	if rem, reset, ok := parseRateHeaders(h); ok && rem == 0 {
		if err := l.observeHeaders(ctx, inst, h); err != nil {
			return Outcome{}, err
		}
		if reset.After(until) {
			until = reset
		}
	}
	giveUp := false
	if until.IsZero() {
		l.mu.Lock()
		l.strikes[inst]++
		n := l.strikes[inst]
		l.mu.Unlock()
		delay := firstBackoff << (n - 1)
		if n > 20 || delay > MaxBackoff {
			delay = MaxBackoff
		}
		until = now.Add(delay)
		giveUp = n > MaxStrikes
	}
	var stored time.Time
	err := l.pool.QueryRow(ctx,
		`INSERT INTO gh_installation_budget AS b (installation_id, remaining, observed_at, backoff_until)
		 VALUES ($1, 0, $2, $3)
		 ON CONFLICT (installation_id) DO UPDATE
		   SET backoff_until = GREATEST(COALESCE(b.backoff_until, '-infinity'::timestamptz), EXCLUDED.backoff_until)
		 RETURNING backoff_until`,
		inst, now.Add(-2*StaleAfter), until).Scan(&stored)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{BackoffUntil: stored, GiveUp: giveUp}, nil
}

// FetcherFunc adapts a function, such as ghsnapshot.Client.RateLimit, to Fetcher.
type FetcherFunc func(ctx context.Context, installationID int64) (remaining int, resetAt time.Time, err error)

func (f FetcherFunc) RateLimit(ctx context.Context, installationID int64) (Snapshot, error) {
	remaining, resetAt, err := f(ctx, installationID)
	return Snapshot{Remaining: remaining, ResetAt: resetAt}, err
}
