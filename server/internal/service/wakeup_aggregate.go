package service

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Aggregate admission (CHE-1082 L7). A scope's starts/hour cap is one counter
// shared by every instance below it. A new paid start takes the row lock of
// each applicable counter, counts that counter's starts in the last hour and is
// admitted only if every counter has room; it then records one reservation per
// counter. Joined firings start no task and never come here.
//
// The counter is keyed by the scope that installed the cap, so an override
// neither creates an ancestor's counter nor lifts its ceiling: all caps apply,
// each at its own scope. A full counter delays the instance, never discards its
// facts; the instance names the counter and when the scheduler looks again.

const (
	// wakeupAggregateWindow is the rolling hour a start counts for.
	wakeupAggregateWindow = time.Hour
	// wakeupAggregateRecheck bounds how long a delayed instance waits before the
	// scheduler looks again, so a slot freed early (a run cancelled before it
	// started) is used within a minute instead of at the window's end.
	wakeupAggregateRecheck = time.Minute
	// wakeupAggregateDefault is the starts/hour a newly created custom root rule
	// gets when it sets none.
	wakeupAggregateDefault = 12
	// wakeupAggregateWaiterStale is how long past its retry time a delayed
	// instance still holds its place in line; one the scheduler has stopped
	// retrying must not keep others waiting.
	wakeupAggregateWaiterStale = 5 * time.Minute
	// wakeupAggregateRetention is how long a reservation is kept: far past the
	// counting window.
	wakeupAggregateRetention = 24 * time.Hour
	// wakeupAggregatePruneBatch bounds the expired reservations one scheduler
	// pass deletes.
	wakeupAggregatePruneBatch = 500
	// maxWakeupDefinitionAggregateLimit bounds an explicit cap.
	maxWakeupDefinitionAggregateLimit = 1000
)

// aggregateStart is the one new paid start asking for admission. FactAge is when
// the oldest fact it would carry arrived, which fixes its place in line: facts
// that were consumed without a start no longer count, so an instance that
// receives a new fact after its old ones were dealt with queues as a new fact.
type aggregateStart struct {
	WorkspaceID, WakeupID, TaskID pgtype.UUID
	RuleKey                       string
	FactAge                       time.Time
}

// factAge is the asking fact's age; a start that names none is as young as now.
func (a aggregateStart) factAge(now time.Time) time.Time {
	if a.FactAge.IsZero() {
		return now
	}
	return a.FactAge
}

// aggregateAdmission is the outcome. When not admitted, Blocked is the counter
// that holds the start back longest and RetryAt when the scheduler should look
// again.
type aggregateAdmission struct {
	Admitted bool
	Blocked  WakeupAggregateCap
	RetryAt  time.Time
}

// admitAggregateStart admits start against every cap or reserves nothing. q must
// be bound to the caller's transaction, which holds the counters' locks until it
// ends. caps are ordered broadest scope first, so racing starts take the locks in
// one order and cannot deadlock on each other. Re-admitting a task that already
// holds a reservation never takes a second slot.
//
// Room is first come, first served. A counter with room still refuses a start
// while older delayed instances are waiting for it, so an event that dispatches
// immediately cannot overtake facts the scheduler is holding back; the oldest
// waiter takes the next slot when it retries.
func admitAggregateStart(ctx context.Context, q *db.Queries, start aggregateStart, caps []WakeupAggregateCap, now time.Time) (aggregateAdmission, error) {
	since := pgtype.Timestamptz{Time: now.Add(-wakeupAggregateWindow), Valid: true}
	for _, c := range caps {
		if _, err := q.LockWakeupAggregateBudget(ctx, db.LockWakeupAggregateBudgetParams{
			WorkspaceID: start.WorkspaceID, ScopeKind: string(c.Scope), ScopeID: c.ScopeID, RuleKey: start.RuleKey, StartsPerHour: int32(c.Limit),
		}); err != nil {
			return aggregateAdmission{}, err
		}
	}
	recheck := now.Add(wakeupAggregateRecheck)
	var out aggregateAdmission
	holdBack := func(c WakeupAggregateCap, until time.Time) {
		// A start waits for the slowest of the counters that refuse it.
		if until.After(out.RetryAt) {
			out.Blocked, out.RetryAt = c, until
		}
	}
	for _, c := range caps {
		sum, err := q.SummarizeWakeupAggregateStarts(ctx, db.SummarizeWakeupAggregateStartsParams{
			WorkspaceID: start.WorkspaceID, ScopeKind: string(c.Scope), ScopeID: c.ScopeID, RuleKey: start.RuleKey, Since: since, TaskID: start.TaskID, StartsPerHour: int64(c.Limit),
		})
		if err != nil {
			return aggregateAdmission{}, err
		}
		if int(sum.Used) >= c.Limit {
			if sum.FreeAt.Valid {
				holdBack(c, sum.FreeAt.Time.Add(wakeupAggregateWindow))
			} else {
				holdBack(c, recheck)
			}
			continue
		}
		older, err := q.CountOlderWakeupAggregateWaiters(ctx, db.CountOlderWakeupAggregateWaitersParams{
			WorkspaceID: start.WorkspaceID, ScopeKind: pgtype.Text{String: string(c.Scope), Valid: true}, ScopeID: c.ScopeID, RuleKey: start.RuleKey,
			WakeupID: start.WakeupID, Since: pgtype.Timestamptz{Time: start.factAge(now), Valid: true},
			FreshAfter: pgtype.Timestamptz{Time: now.Add(-wakeupAggregateWaiterStale), Valid: true},
		})
		if err != nil {
			return aggregateAdmission{}, err
		}
		if int(sum.Used)+int(older) >= c.Limit {
			holdBack(c, recheck)
		}
	}
	if out.RetryAt.IsZero() {
		for _, c := range caps {
			if err := q.ReserveWakeupAggregateStart(ctx, db.ReserveWakeupAggregateStartParams{
				WorkspaceID: start.WorkspaceID, ScopeKind: string(c.Scope), ScopeID: c.ScopeID, RuleKey: start.RuleKey, TaskID: start.TaskID, WakeupID: start.WakeupID,
				ReservedAt: pgtype.Timestamptz{Time: now, Valid: true},
			}); err != nil {
				return aggregateAdmission{}, err
			}
		}
		out.Admitted = true
		return out, nil
	}
	if recheck.Before(out.RetryAt) {
		out.RetryAt = recheck
	}
	return out, nil
}

// pruneWakeupAggregateReservations deletes one bounded batch of reservations
// older than the retention window. Only starts inside the one-hour counting
// window matter, so a day of margin leaves a retried admission nothing to
// double count.
func pruneWakeupAggregateReservations(ctx context.Context, q *db.Queries, now time.Time) error {
	_, err := q.PruneWakeupAggregateReservations(ctx, db.PruneWakeupAggregateReservationsParams{
		Before: pgtype.Timestamptz{Time: now.Add(-wakeupAggregateRetention), Valid: true}, Batch: wakeupAggregatePruneBatch,
	})
	return err
}

// WakeupAggregateStatus is one counter's state as data for the layers that
// show it: starts counted in the last hour, its limit, the instances it is
// holding back and the earliest time the scheduler looks at any of them again.
// The retry time is when a delayed start is next considered, not a promise that
// it runs then.
type WakeupAggregateStatus struct {
	Used, Limit, Backlog int
	EarliestRetry        time.Time
}

// WakeupAggregateStatusOf reads one counter without locking it.
func WakeupAggregateStatusOf(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, c WakeupAggregateCap, ruleKey string, now time.Time) (WakeupAggregateStatus, error) {
	sum, err := q.SummarizeWakeupAggregateStarts(ctx, db.SummarizeWakeupAggregateStartsParams{
		WorkspaceID: workspaceID, ScopeKind: string(c.Scope), ScopeID: c.ScopeID, RuleKey: ruleKey,
		Since: pgtype.Timestamptz{Time: now.Add(-wakeupAggregateWindow), Valid: true}, TaskID: pgtype.UUID{Valid: true}, // no task is excluded
		StartsPerHour: int64(c.Limit),
	})
	if err != nil {
		return WakeupAggregateStatus{}, err
	}
	held, err := q.SummarizeWakeupAggregateBacklog(ctx, db.SummarizeWakeupAggregateBacklogParams{WorkspaceID: workspaceID, ScopeKind: pgtype.Text{String: string(c.Scope), Valid: true}, ScopeID: c.ScopeID, RuleKey: ruleKey})
	if err != nil {
		return WakeupAggregateStatus{}, err
	}
	status := WakeupAggregateStatus{Used: int(sum.Used), Limit: c.Limit, Backlog: int(held.Backlog)}
	if held.EarliestRetry.Valid {
		status.EarliestRetry = held.EarliestRetry.Time
	}
	return status, nil
}

// aggregateCaps are the caps an instance's starts count against; none for the
// legacy path and for a rule whose chain sets no cap.
func (b *builtinWakeup) aggregateCaps() []WakeupAggregateCap {
	if b == nil {
		return nil
	}
	return b.Eff.AggregateCaps
}

// withRootAggregateDefault gives a newly created custom root rule the 12/hour
// cap it did not set. Only creation calls it: updates, built-ins and overrides
// never gain a default.
func withRootAggregateDefault(p *WakeupConfigPatch) {
	if !p.AggregateLimit.Set {
		p.AggregateLimit = wakeupField[int]{Set: true, Value: wakeupAggregateDefault}
	}
}

// checkWakeupAggregateScope refuses a cap an issue could only use to duplicate
// its own rate_limit; counters belong to the workspace or a project.
func checkWakeupAggregateScope(kind WakeupScope, p WakeupConfigPatch) error {
	if kind == WakeupScopeIssue && p.AggregateLimit.Set {
		return wakeupDefinitionBad("aggregate_limit is set at a workspace or project; an issue is bounded by rate_limit")
	}
	return nil
}

// aggregateCounterBusy reports a row lock that stayed busy past the dispatch's short
// lock_timeout: the start is simply tried again on a later pass.
func aggregateCounterBusy(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "55P03"
}

// oldestReceiptAge is when the oldest of the facts arrived; zero when there are none.
func oldestReceiptAge(receipts []db.IssueWakeupReceipt) time.Time {
	var oldest time.Time
	for _, r := range receipts {
		if r.CreatedAt.Valid && (oldest.IsZero() || r.CreatedAt.Time.Before(oldest)) {
			oldest = r.CreatedAt.Time
		}
	}
	return oldest
}
