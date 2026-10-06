package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Activation sweep (CHE-1082 L10). A condition rule has to reach the issues that
// already exist when its definition is written, and keep reaching the ones that
// become eligible later. The sweep walks a definition's scope in keyset order,
// wakeupSweepBatch issues per scheduler pass, each issue in its own short
// transaction under that issue's row lock and no workspace-wide lock. Its state
// is on the definition (migration 598): the revision it belongs to, a persisted
// cursor, and the lease and retry bookkeeping. A revised or deleted definition
// ends the old sweep by revision alone; the old cursor is never reused. Issues
// that become eligible after the sweep has passed them reach the rule through
// the activation signal (wakeupActivateEvent), which the L9 drain handles.
const (
	wakeupSweepBatch       = 50
	wakeupSweepLease       = 2 * time.Minute
	wakeupSweepMaxAttempts = 5
	wakeupSweepRetryBase   = 30 * time.Second
	// wakeupSweepSettle bounds how many definitions that need no walk (events,
	// built-ins) one pass settles before it looks at the next real batch.
	wakeupSweepSettle = 20
	// wakeupCapacityRetryBatch bounds the held-back instances one pass retries.
	wakeupCapacityRetryBatch = 50
)

// SweepWakeupDefinitions runs one bounded sweep pass over the given
// workspaces. Tests use it; the scheduler's tick sweeps every workspace.
func (s *IssueWakeupService) SweepWakeupDefinitions(ctx context.Context, workspaceIDs ...pgtype.UUID) error {
	if len(workspaceIDs) == 0 {
		return nil
	}
	return s.sweepDefinitions(ctx, workspaceIDs)
}

// sweepDefinitions is one pass: it settles definitions that need no walk, walks
// one batch of the first one that does, and retries instances a full default
// pool held back.
func (s *IssueWakeupService) sweepDefinitions(ctx context.Context, workspaceIDs []pgtype.UUID) error {
	var errs []error
	for range wakeupSweepSettle {
		if ctx.Err() != nil {
			return errors.Join(append(errs, ctx.Err())...)
		}
		outcome, err := s.sweepOne(ctx, workspaceIDs)
		if err != nil {
			errs = append(errs, err)
		}
		if outcome != sweepSettled || err != nil {
			break
		}
	}
	if err := s.retryCapacityHeld(ctx, workspaceIDs); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

type sweepOutcome int

const (
	sweepNone    sweepOutcome = iota // nothing was pending
	sweepSettled                     // a definition that needs no walk was marked done
	sweepWalked                      // a batch of issues was examined
)

// sweepOne leases one pending definition and works it.
func (s *IssueWakeupService) sweepOne(ctx context.Context, workspaceIDs []pgtype.UUID) (sweepOutcome, error) {
	now := time.Now()
	d, err := s.Tasks.Queries.ClaimWakeupDefinitionSweep(ctx, db.ClaimWakeupDefinitionSweepParams{
		Now: pgtype.Timestamptz{Time: now, Valid: true}, WorkspaceIds: workspaceIDs, LeaseUntil: pgtype.Timestamptz{Time: now.Add(wakeupSweepLease), Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return sweepNone, nil
	}
	if err != nil {
		return sweepNone, err
	}
	finished, last, err := s.sweepBatch(ctx, d)
	if err != nil {
		retry := now.Add(wakeupSweepRetryBase << min(d.SweepAttempts, 6))
		_, failErr := s.Tasks.Queries.FailWakeupDefinitionSweep(ctx, db.FailWakeupDefinitionSweepParams{
			WorkspaceID: d.WorkspaceID, ScopeKind: d.ScopeKind, ScopeID: d.ScopeID, RuleKey: d.RuleKey, Revision: d.Revision,
			Error: truncateForSummary(err.Error(), 500), RetryAt: pgtype.Timestamptz{Time: retry, Valid: true}, MaxAttempts: wakeupSweepMaxAttempts,
		})
		return sweepWalked, errors.Join(fmt.Errorf("sweep wakeup definition %s: %w", d.RuleKey, err), failErr)
	}
	_, err = s.Tasks.Queries.AdvanceWakeupDefinitionSweep(ctx, db.AdvanceWakeupDefinitionSweepParams{
		WorkspaceID: d.WorkspaceID, ScopeKind: d.ScopeKind, ScopeID: d.ScopeID, RuleKey: d.RuleKey, Revision: d.Revision,
		Cursor: last, Done: finished,
	})
	if err == nil && !sweepsRule(d) {
		return sweepSettled, nil
	}
	return sweepWalked, err
}

// sweepsRule reports whether a definition's rule has instances a walk must
// create or reconcile: a custom rule that is, or may override, a condition.
// Events create their instances on the first event, and a built-in has its own.
func sweepsRule(d db.IssueWakeupDefinition) bool {
	if _, builtin := WakeupBuiltinBaseline(d.RuleKey); builtin {
		return false
	}
	if !d.Root {
		return true // an override may enable or retire a condition the root defines
	}
	def, err := WakeupDefinitionFromRow(d)
	return err == nil && customCondition(def.Patch.Trigger) != nil
}

// sweepBatch examines the next wakeupSweepBatch issues of the definition's scope
// after its cursor. last is the cursor to persist: invalid once the scope is
// finished, so a finished sweep never leaves one behind.
func (s *IssueWakeupService) sweepBatch(ctx context.Context, d db.IssueWakeupDefinition) (finished bool, last pgtype.UUID, err error) {
	if !sweepsRule(d) {
		return true, pgtype.UUID{}, nil
	}
	var ids []pgtype.UUID
	switch WakeupScope(d.ScopeKind) {
	case WakeupScopeIssue:
		ids = []pgtype.UUID{d.ScopeID}
	case WakeupScopeProject:
		ids, err = s.Tasks.Queries.ListSweepProjectIssues(ctx, db.ListSweepProjectIssuesParams{ProjectID: d.ScopeID, WorkspaceID: d.WorkspaceID, After: sweepAfter(d.SweepCursor), BatchSize: wakeupSweepBatch})
	default:
		ids, err = s.Tasks.Queries.ListSweepWorkspaceIssues(ctx, db.ListSweepWorkspaceIssuesParams{WorkspaceID: d.WorkspaceID, After: sweepAfter(d.SweepCursor), BatchSize: wakeupSweepBatch})
	}
	if err != nil {
		return false, pgtype.UUID{}, err
	}
	// The cursor only moves past issues that were handled. An issue another
	// writer holds is tried again by the next pass, never skipped.
	last = d.SweepCursor
	for _, id := range ids {
		if err := s.activateIssueRule(ctx, d.WorkspaceID, id, d.RuleKey, d.SweepBaseline); scopedLockBusy(err) {
			return false, last, nil
		} else if err != nil {
			return false, pgtype.UUID{}, err
		}
		last = id
	}
	if finished = len(ids) < wakeupSweepBatch; finished {
		last = pgtype.UUID{}
	}
	return finished, last, nil
}

// sweepAfter is the keyset position to read after: the zero UUID sorts before
// every other.
func sweepAfter(cursor pgtype.UUID) pgtype.UUID {
	if cursor.Valid {
		return cursor
	}
	return pgtype.UUID{Valid: true}
}

// activateIssueRule brings one issue's instance of a condition rule in line with
// the rule as it resolves now, in one short transaction under the issue's row
// lock. An issue the rule does not run for (closed, parked in backlog, another
// project) is left alone, and an instance it has is retired.
func (s *IssueWakeupService) activateIssueRule(ctx context.Context, ws, issueID pgtype.UUID, key string, baseline bool) error {
	tx, err := s.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '200ms'"); err != nil {
		return err
	}
	q := s.Tasks.Queries.WithTx(tx)
	b, err := s.newScopedIssueBatch(ctx, tx, q, db.WakeupScopedEvent{IssueID: issueID, WorkspaceID: ws})
	if err != nil {
		return err
	}
	if b.outcome != "" || b.rules[key] == nil {
		return nil
	}
	if _, _, err := b.activate(ctx, b.rules[key], baseline); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.publishWithdrawn(ctx, b.withdrawn)
	return nil
}

// activate ensures the rule's instance on the batch's issue when the rule is a
// condition that runs there, and retires an existing one when the rule does not
// run there at all. An event rule's instance is created by its first event and
// is none of the sweep's business. It reports the instance, or the reason there
// is none.
func (b *scopedIssueBatch) activate(ctx context.Context, r *scopedRule, baseline bool) (db.IssueWakeup, string, error) {
	if why := r.notRunning(); why != "" {
		existing, err := b.q.GetDefaultWakeupInstance(ctx, db.GetDefaultWakeupInstanceParams{IssueID: b.issue.ID, RuleKey: r.key})
		if errors.Is(err, pgx.ErrNoRows) {
			return db.IssueWakeup{}, why, nil
		}
		if err != nil {
			return db.IssueWakeup{}, "", err
		}
		return db.IssueWakeup{}, why, b.retire(ctx, existing)
	}
	if r.condition == nil {
		return db.IssueWakeup{}, scopedOutcomeNoMatch, nil
	}
	return b.ensure(ctx, r, baseline)
}

// notRunning says why a rule does not run for the issue at all (unreadable, no
// root in the issue's chain, disabled); empty when it does.
func (r *scopedRule) notRunning() string {
	switch {
	case r.invalid:
		return scopedOutcomeInvalid
	case !r.eff.Applicable:
		return scopedOutcomeNoRule
	case !r.eff.Enabled():
		return scopedOutcomeRuleDisabled
	}
	return ""
}

// retryCapacityHeld applies instances a full default pool left unapplied, now
// that capacity may have freed. A refused attempt touches the instance, so every
// held instance gets its turn and none is retried every pass.
func (s *IssueWakeupService) retryCapacityHeld(ctx context.Context, workspaceIDs []pgtype.UUID) error {
	held, err := s.Tasks.Queries.ListCapacityHeldWakeups(ctx, db.ListCapacityHeldWakeupsParams{WorkspaceIds: workspaceIDs, BatchSize: wakeupCapacityRetryBatch})
	if err != nil {
		return err
	}
	var errs []error
	for _, h := range held {
		if err := s.retryCapacityHeldOne(ctx, h.IssueID, h.ID); err != nil && !scopedLockBusy(err) {
			errs = append(errs, fmt.Errorf("retry held wakeup %s: %w", util.UUIDToString(h.ID), err))
		}
	}
	return errors.Join(errs...)
}

func (s *IssueWakeupService) retryCapacityHeldOne(ctx context.Context, issueID, id pgtype.UUID) error {
	tx, err := s.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '200ms'"); err != nil {
		return err
	}
	q := s.Tasks.Queries.WithTx(tx)
	if _, err := q.LockWakeupIssue(ctx, issueID); errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	w, err := q.LockIssueWakeup(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (w.Enabled || !w.CapacityReason.Valid) {
		return nil
	} else if err != nil {
		return err
	}
	if customInstanceEnded(w) {
		// It rests for good: it stops waiting for capacity, never to be applied.
		if err := q.ClearEndedWakeupCapacityReason(ctx, w.ID); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if _, err := ApplyDefaultWakeupInstance(ctx, tx, w.ID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
