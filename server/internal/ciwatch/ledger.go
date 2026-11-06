package ciwatch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/ghbudget"
)

const (
	StateScheduled = "scheduled"
	StateExecuting = "executing"
	StateDone      = "done"
	StateFailed    = "failed"
	StateCancelled = "cancelled"
	StateAmbiguous = "ambiguous"
)

const (
	ActionRerun        = "rerun"
	ActionIssueCreate  = "issue_create"
	ActionIssueComment = "issue_comment"
	ActionInboxNotify  = "inbox_notify"
)

const (
	ReasonManualRerun = "manual_rerun"
	ReasonStale       = "stale"
	ReasonOptedOut    = "opted_out"
)

const (
	ReasonAttemptsExhausted = "attempts_exhausted"
	ReasonReconcileNotFound = "reconcile_not_found"
	ReasonLeaseExpired      = "lease_expired"
	ReasonPostError         = "post_error"
	ReasonReconciled        = "reconciled"
)

const (
	StaleAfter   = 6 * time.Hour
	MaxAttempts  = 5
	LeaseTTL     = 2 * time.Minute
	ReconcileGap = 2 * time.Minute
	MaxReconcile = 2
)

type NewAction struct {
	WorkspaceID, RepoOwner, RepoName string
	OriginalRunID                    int64
	Kind, Protocol, SignatureHash    string
	DueAt, RunCompletedAt            time.Time
}

type Action struct {
	ID, WorkspaceID, RepoOwner, RepoName string
	OriginalRunID                        int64
	Kind, State, Protocol, SignatureHash string
	LeaseEpoch                           int64
	DueAt, RunCompletedAt                time.Time
	LeaseUntil, PostStartedAt            *time.Time
	Attempts, ReconcileAttempts          int
	ExpectedAttempt                      *int
	DestinationID, Reason, Diagnostic    string
}

type Verdict struct {
	CancelReason string
	DeferUntil   time.Time
}

type (
	Recheck     func(ctx context.Context, tx pgx.Tx, a Action) (Verdict, error)
	LocalEffect func(ctx context.Context, tx pgx.Tx, a Action) (destinationID string, err error)
)

type RemoteOutcome struct{ State, DestinationID, Reason string }

type RemoteExecutor interface {
	Execute(ctx context.Context, a Action, mark func(expectedAttempt int) error) (RemoteOutcome, error)
	Reconcile(ctx context.Context, a Action) (out RemoteOutcome, found bool, err error)
}

type PermanentError struct {
	Reason string
	Err    error
}

func (e *PermanentError) Error() string { return e.Reason + ": " + e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

type CancelFilter struct{ WorkspaceID, RepoOwner, RepoName, Protocol string }

// Ledger owns the ci_watch_action lifecycle (CHE-4277). Local kinds (issue,
// comment, notification) claim and run in one transaction, so `executing` is
// never committed for them. The one remote kind (rerun) POSTs at most once.
type Ledger struct {
	pool    *pgxpool.Pool
	recheck Recheck
	now     func() time.Time

	mu     sync.RWMutex
	local  map[string]LocalEffect
	remote map[string]RemoteExecutor
}

type LedgerOption func(*Ledger)

func WithLedgerClock(now func() time.Time) LedgerOption { return func(l *Ledger) { l.now = now } }

// NewLedger: recheck runs inside the claim transaction before every side
// effect; nil means nothing to check.
func NewLedger(pool *pgxpool.Pool, recheck Recheck, opts ...LedgerOption) *Ledger {
	l := &Ledger{pool: pool, recheck: recheck, now: time.Now,
		local: map[string]LocalEffect{}, remote: map[string]RemoteExecutor{}}
	for _, o := range opts {
		o(l)
	}
	return l
}

func (l *Ledger) RegisterLocal(kind string, e LocalEffect) {
	l.mu.Lock()
	l.local[kind] = e
	l.mu.Unlock()
}

func (l *Ledger) RegisterRemote(kind string, e RemoteExecutor) {
	l.mu.Lock()
	l.remote[kind] = e
	l.mu.Unlock()
}

// remoteActionKinds are the kinds whose effect leaves this database. Recovery
// classifies by this list, never by what is registered at restart time.
var remoteActionKinds = []string{ActionRerun}

// registeredKinds: a row of a kind with no executor yet waits (a restart can
// run before registration); it is never failed or sent anywhere.
func (l *Ledger) registeredKinds() []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	kinds := make([]string, 0, len(l.local)+len(l.remote))
	for k := range l.local {
		kinds = append(kinds, k)
	}
	for k := range l.remote {
		kinds = append(kinds, k)
	}
	return kinds
}

// remoteKinds are the remote kinds that have an executor registered.
func (l *Ledger) remoteKinds() []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	kinds := make([]string, 0, len(l.remote))
	for k := range l.remote {
		kinds = append(kinds, k)
	}
	return kinds
}

var errFenced = errors.New("ciwatch: lease epoch changed")

const actionCols = `id::text, workspace_id::text, repo_owner, repo_name, original_run_id, action_kind, state,
	lease_epoch, protocol, signature_hash, due_at, run_completed_at, lease_until, attempts,
	reconcile_attempts, expected_attempt, post_started_at, COALESCE(destination_id,''), reason, diagnostic`

func scanAction(r pgx.Row) (a Action, err error) {
	err = r.Scan(&a.ID, &a.WorkspaceID, &a.RepoOwner, &a.RepoName, &a.OriginalRunID, &a.Kind, &a.State,
		&a.LeaseEpoch, &a.Protocol, &a.SignatureHash, &a.DueAt, &a.RunCompletedAt, &a.LeaseUntil, &a.Attempts,
		&a.ReconcileAttempts, &a.ExpectedAttempt, &a.PostStartedAt, &a.DestinationID, &a.Reason, &a.Diagnostic)
	return a, err
}

// diagnostic is the bounded, redacted error text a terminal row carries.
func diagnostic(err error) string {
	if err == nil {
		return ""
	}
	r := []rune(redactTokens(err.Error()))
	if len(r) > 200 {
		r = r[:200]
	}
	return string(r)
}

func (l *Ledger) Get(ctx context.Context, id string) (Action, error) {
	return scanAction(l.pool.QueryRow(ctx, `SELECT `+actionCols+` FROM ci_watch_action WHERE id=$1::uuid`, id))
}

// queryer is satisfied by both the pool and a transaction.
type queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Schedule claims (repo, original run, kind). inserted=false returns the
// existing row; only the inserting caller owns the action.
func (l *Ledger) Schedule(ctx context.Context, n NewAction) (Action, bool, error) {
	if n.Kind == ActionIssueCreate {
		return Action{}, false, errors.New("ciwatch: issue_create goes through ClaimAndSchedule")
	}
	return l.schedule(ctx, l.pool, n)
}

// ClaimAndSchedule is the only way to create an issue_create action. In one
// transaction it claims the signature owner and inserts n. claimed=false means
// nothing new was committed: the signature is already owned, or this
// (repo, run, kind) was scheduled before (duplicate delivery, even after a
// cancel) and the existing action is returned. A crash leaves neither row.
func (l *Ledger) ClaimAndSchedule(ctx context.Context, n NewAction, reopenAfter time.Duration) (Action, bool, error) {
	if n.Kind != ActionIssueCreate || n.SignatureHash == "" {
		return Action{}, false, errors.New("ciwatch: ClaimAndSchedule needs an issue_create action with a signature")
	}
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return Action{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	owned, err := l.claimOwner(ctx, tx, n.WorkspaceID, n.RepoOwner, n.RepoName, n.SignatureHash, reopenAfter)
	if err != nil {
		return Action{}, false, err
	}
	if !owned {
		a, err := l.existing(ctx, tx, n)
		return a, false, err
	}
	a, inserted, err := l.schedule(ctx, tx, n)
	if err != nil || !inserted {
		return a, false, err // deferred Rollback drops the new claim
	}
	return a, true, tx.Commit(ctx)
}

func (l *Ledger) schedule(ctx context.Context, q queryer, n NewAction) (Action, bool, error) {
	var id string
	err := q.QueryRow(ctx, `
		INSERT INTO ci_watch_action (workspace_id, repo_owner, repo_name, original_run_id, action_kind,
			protocol, signature_hash, due_at, run_completed_at, created_at, updated_at)
		VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10)
		ON CONFLICT (workspace_id, repo_owner, repo_name, original_run_id, action_kind) DO NOTHING
		RETURNING id::text`,
		n.WorkspaceID, n.RepoOwner, n.RepoName, n.OriginalRunID, n.Kind, n.Protocol, n.SignatureHash,
		n.DueAt, n.RunCompletedAt, l.now()).Scan(&id)
	inserted := err == nil
	if errors.Is(err, pgx.ErrNoRows) {
		err = q.QueryRow(ctx, `SELECT id::text FROM ci_watch_action
			WHERE workspace_id=$1::uuid AND repo_owner=$2 AND repo_name=$3 AND original_run_id=$4 AND action_kind=$5`,
			n.WorkspaceID, n.RepoOwner, n.RepoName, n.OriginalRunID, n.Kind).Scan(&id)
	}
	if err != nil {
		return Action{}, false, err
	}
	a, err := scanAction(q.QueryRow(ctx, `SELECT `+actionCols+` FROM ci_watch_action WHERE id=$1::uuid`, id))
	return a, inserted, err
}

// existing returns the action already scheduled for n's key, or the zero
// Action when there is none.
func (l *Ledger) existing(ctx context.Context, q queryer, n NewAction) (Action, error) {
	a, err := scanAction(q.QueryRow(ctx, `SELECT `+actionCols+` FROM ci_watch_action
		WHERE workspace_id=$1::uuid AND repo_owner=$2 AND repo_name=$3 AND original_run_id=$4 AND action_kind=$5`,
		n.WorkspaceID, n.RepoOwner, n.RepoName, n.OriginalRunID, n.Kind))
	if errors.Is(err, pgx.ErrNoRows) {
		return Action{}, nil
	}
	return a, err
}

// settle ends a row held by tx, fenced on its epoch. A non-done end of an
// issue_create frees the `creating` owner it was holding.
func (l *Ledger) settle(ctx context.Context, tx pgx.Tx, a Action, from, state, reason, diag, dest string) error {
	tag, err := tx.Exec(ctx, `UPDATE ci_watch_action SET state=$2, reason=$3, diagnostic=$4,
		destination_id=NULLIF($5,''), lease_until=NULL, updated_at=$6
		WHERE id=$1::uuid AND lease_epoch=$7 AND state=$8`,
		a.ID, state, reason, diag, dest, l.now(), a.LeaseEpoch, from)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errFenced
	}
	if state != StateDone {
		return l.releaseOwner(ctx, tx, a)
	}
	return nil
}

func (l *Ledger) releaseOwner(ctx context.Context, tx pgx.Tx, a Action) error {
	if a.Kind != ActionIssueCreate || a.SignatureHash == "" {
		return nil
	}
	_, err := tx.Exec(ctx, `DELETE FROM ci_signature_owner WHERE workspace_id=$1::uuid AND repo_owner=$2
		AND repo_name=$3 AND signature_hash=$4 AND state='creating' AND issue_id IS NULL`,
		a.WorkspaceID, a.RepoOwner, a.RepoName, a.SignatureHash)
	return err
}

// RunOne takes one due row. ran=false means nothing was due.
func (l *Ledger) RunOne(ctx context.Context) (bool, error) {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now := l.now()
	a, err := scanAction(tx.QueryRow(ctx, `SELECT `+actionCols+` FROM ci_watch_action
		WHERE state='scheduled' AND due_at<=$1 AND action_kind = ANY($2)
		ORDER BY due_at LIMIT 1 FOR UPDATE SKIP LOCKED`, now, l.registeredKinds()))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	after, err := l.run(ctx, tx, a, now)
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		_ = tx.Rollback(ctx)
		if ferr := l.recordFailure(ctx, a, err); ferr != nil {
			err = errors.Join(err, ferr)
		}
		return true, err
	}
	if after != nil {
		return true, after()
	}
	return true, nil
}

// run claims a (epoch+1) and decides inside tx. Local kinds finish here; a
// remote kind commits `executing`; `after` then sends its one POST.
func (l *Ledger) run(ctx context.Context, tx pgx.Tx, a Action, now time.Time) (after func() error, err error) {
	if now.Sub(a.RunCompletedAt) > StaleAfter {
		return nil, l.settle(ctx, tx, a, StateScheduled, StateCancelled, ReasonStale, "", "")
	}
	var v Verdict
	if l.recheck != nil {
		v, err = l.recheck(ctx, tx, a)
		var de *ghbudget.DeferredError
		if errors.As(err, &de) {
			v, err = Verdict{DeferUntil: de.DueAt}, nil
		}
		if err != nil {
			return nil, fmt.Errorf("recheck: %w", err)
		}
	}
	switch {
	case v.CancelReason != "":
		return nil, l.settle(ctx, tx, a, StateScheduled, StateCancelled, v.CancelReason, "", "")
	case !v.DeferUntil.IsZero():
		_, err = tx.Exec(ctx, `UPDATE ci_watch_action SET due_at=$2, updated_at=$3 WHERE id=$1::uuid`, a.ID, v.DeferUntil, now)
		return nil, err
	}

	l.mu.RLock()
	effect := l.local[a.Kind]
	remote, isRemote := l.remote[a.Kind]
	l.mu.RUnlock()
	switch {
	case isRemote:
		return l.claimRemote(ctx, tx, a, remote, now)
	}

	a.LeaseEpoch++
	if _, err = tx.Exec(ctx, `UPDATE ci_watch_action SET lease_epoch=$2 WHERE id=$1::uuid`, a.ID, a.LeaseEpoch); err != nil {
		return nil, err
	}
	withOwner := a.Kind == ActionIssueCreate && a.SignatureHash != ""
	if withOwner {
		creating, err := l.lockOwner(ctx, tx, a)
		if err != nil {
			return nil, err
		}
		if !creating {
			return nil, l.settle(ctx, tx, a, StateScheduled, StateCancelled, ReasonOwnedElsewhere, "", "")
		}
	}
	dest, err := effect(ctx, tx, a)
	if err != nil {
		return nil, err
	}
	if withOwner {
		tag, err := tx.Exec(ctx, `UPDATE ci_signature_owner SET issue_id=$5::uuid, state='open'
			WHERE workspace_id=$1::uuid AND repo_owner=$2 AND repo_name=$3 AND signature_hash=$4 AND state='creating'`,
			a.WorkspaceID, a.RepoOwner, a.RepoName, a.SignatureHash, dest)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() != 1 {
			return nil, errFenced
		}
	}
	return nil, l.settle(ctx, tx, a, StateScheduled, StateDone, "", "", dest)
}

func ownerKey(ws, owner, repo, sig string) string { return ws + "/" + owner + "/" + repo + "/" + sig }

// lockOwner takes the (repo, signature) advisory lock and the owner row; it
// reports whether the row is still `creating`.
func (l *Ledger) lockOwner(ctx context.Context, tx pgx.Tx, a Action) (bool, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		ownerKey(a.WorkspaceID, a.RepoOwner, a.RepoName, a.SignatureHash)); err != nil {
		return false, err
	}
	var state string
	err := tx.QueryRow(ctx, `SELECT state FROM ci_signature_owner WHERE workspace_id=$1::uuid AND repo_owner=$2
		AND repo_name=$3 AND signature_hash=$4 FOR UPDATE`,
		a.WorkspaceID, a.RepoOwner, a.RepoName, a.SignatureHash).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return state == "creating", err
}

// recordFailure runs after the claim transaction rolled back: one more
// attempt later, or a terminal state.
func (l *Ledger) recordFailure(ctx context.Context, a Action, cause error) error {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var perm *PermanentError
	attempts := a.Attempts + 1
	if _, err = tx.Exec(ctx, `UPDATE ci_watch_action SET attempts=$2 WHERE id=$1::uuid AND lease_epoch=$3`,
		a.ID, attempts, a.LeaseEpoch); err != nil {
		return err
	}
	switch {
	case errors.As(cause, &perm):
		err = l.settle(ctx, tx, a, StateScheduled, StateFailed, perm.Reason, diagnostic(cause), "")
	case attempts >= MaxAttempts:
		err = l.settle(ctx, tx, a, StateScheduled, StateFailed, ReasonAttemptsExhausted, diagnostic(cause), "")
	default:
		_, err = tx.Exec(ctx, `UPDATE ci_watch_action SET due_at=$2, diagnostic=$3, updated_at=$4
			WHERE id=$1::uuid AND lease_epoch=$5 AND state='scheduled'`,
			a.ID, l.now().Add(time.Duration(attempts)*time.Minute), diagnostic(cause), l.now(), a.LeaseEpoch)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// claimRemote commits `executing` (2-minute lease, epoch+1); the returned
// closure then sends the single POST. It never runs twice for a row: a
// scheduled row only reaches here once, and anything after the commit ends in
// done, failed or ambiguous, never scheduled.
func (l *Ledger) claimRemote(ctx context.Context, tx pgx.Tx, a Action, ex RemoteExecutor, now time.Time) (func() error, error) {
	a.LeaseEpoch++
	a.State = StateExecuting
	if _, err := tx.Exec(ctx, `UPDATE ci_watch_action SET state='executing', lease_epoch=$2, attempts=attempts+1,
		lease_until=$3, updated_at=$4 WHERE id=$1::uuid`, a.ID, a.LeaseEpoch, now.Add(LeaseTTL), now); err != nil {
		return nil, err
	}
	return func() error { return l.postRemote(ctx, a, ex) }, nil
}

func (l *Ledger) postRemote(ctx context.Context, a Action, ex RemoteExecutor) error {
	mark := func(expected int) error {
		tag, err := l.pool.Exec(ctx, `UPDATE ci_watch_action SET expected_attempt=$2, post_started_at=$3, updated_at=$3
			WHERE id=$1::uuid AND lease_epoch=$4 AND state='executing'`, a.ID, expected, l.now(), a.LeaseEpoch)
		if err == nil && tag.RowsAffected() != 1 {
			err = errFenced
		}
		return err
	}
	out, err := ex.Execute(ctx, a, mark)
	if err == nil && out.State != StateDone && out.State != StateFailed {
		err = fmt.Errorf("executor returned state %q", out.State)
	}
	now := l.now()
	if err != nil {
		_, uerr := l.pool.Exec(ctx, `UPDATE ci_watch_action SET state='ambiguous', reason=$2, diagnostic=$3,
			due_at=$4, lease_until=NULL, updated_at=$5 WHERE id=$1::uuid AND lease_epoch=$6 AND state='executing'`,
			a.ID, ReasonPostError, diagnostic(err), now.Add(ReconcileGap), now, a.LeaseEpoch)
		return uerr
	}
	// A 0-row update means recovery already moved the row; reconcile owns it.
	_, uerr := l.pool.Exec(ctx, `UPDATE ci_watch_action SET state=$2, reason=$3, destination_id=NULLIF($4,''),
		lease_until=NULL, updated_at=$5 WHERE id=$1::uuid AND lease_epoch=$6 AND state='executing'`,
		a.ID, out.State, out.Reason, out.DestinationID, now, a.LeaseEpoch)
	return uerr
}

// Sweep recovers `executing` rows past their lease that nobody holds. Remote
// kinds become ambiguous (read-only reconcile); local kinds roll back to
// scheduled under a new epoch. Rows locked by a live transaction are skipped.
func (l *Ledger) Sweep(ctx context.Context) (int, error) {
	now, remote := l.now(), remoteActionKinds
	const lockedExpired = `id IN (SELECT id FROM ci_watch_action WHERE state='executing' AND lease_until<$1
		AND %s FOR UPDATE SKIP LOCKED)`
	amb, err := l.pool.Exec(ctx, `UPDATE ci_watch_action SET state='ambiguous', reason=$3, lease_until=NULL,
		due_at=$2, updated_at=$1 WHERE `+fmt.Sprintf(lockedExpired, "action_kind = ANY($4)"),
		now, now.Add(ReconcileGap), ReasonLeaseExpired, remote)
	if err != nil {
		return 0, err
	}
	loc, err := l.pool.Exec(ctx, `UPDATE ci_watch_action SET state='scheduled', lease_epoch=lease_epoch+1,
		lease_until=NULL, updated_at=$1 WHERE `+fmt.Sprintf(lockedExpired, "action_kind <> ALL($2)"), now, remote)
	if err != nil {
		return int(amb.RowsAffected()), err
	}
	return int(amb.RowsAffected() + loc.RowsAffected()), nil
}

// ReconcileOne reads (never POSTs) the outcome of one due ambiguous row.
func (l *Ledger) ReconcileOne(ctx context.Context) (bool, error) {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now := l.now()
	a, err := scanAction(tx.QueryRow(ctx, `SELECT `+actionCols+` FROM ci_watch_action
		WHERE state='ambiguous' AND due_at<=$1 AND action_kind = ANY($2)
		ORDER BY due_at LIMIT 1 FOR UPDATE SKIP LOCKED`, now, l.remoteKinds()))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	l.mu.RLock()
	ex := l.remote[a.Kind]
	l.mu.RUnlock()
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, found, rerr := ex.Reconcile(rctx, a)
	if rerr == nil && found && (out.State == StateDone || out.State == StateFailed) {
		reason := out.Reason
		if reason == "" {
			reason = ReasonReconciled
		}
		_, err = tx.Exec(ctx, `UPDATE ci_watch_action SET state=$2, reason=$3, destination_id=NULLIF($4,''),
			lease_epoch=lease_epoch+1, updated_at=$5 WHERE id=$1::uuid`, a.ID, out.State, reason, out.DestinationID, now)
	} else {
		n := a.ReconcileAttempts + 1
		state, reason := StateAmbiguous, a.Reason
		if n >= MaxReconcile {
			state, reason = StateFailed, ReasonReconcileNotFound
		}
		_, err = tx.Exec(ctx, `UPDATE ci_watch_action SET state=$2, reason=$3, reconcile_attempts=$4, due_at=$5,
			diagnostic=CASE WHEN $6 = '' THEN diagnostic ELSE $6 END, lease_epoch=lease_epoch+1, updated_at=$7
			WHERE id=$1::uuid`, a.ID, state, reason, n, now.Add(ReconcileGap), diagnostic(rerr), now)
	}
	if err != nil {
		return true, err
	}
	return true, tx.Commit(ctx)
}

// CancelScheduled cancels not-yet-claimed rows of a repo (optionally one
// protocol), for opt-out or a mode change during the delay. Rows a worker
// holds are re-evaluated after its commit, so a finished row stays done.
func (l *Ledger) CancelScheduled(ctx context.Context, f CancelFilter, reason string) (int, error) {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `UPDATE ci_watch_action SET state='cancelled', reason=$5, updated_at=$6
		WHERE state='scheduled' AND workspace_id=$1::uuid AND repo_owner=$2 AND repo_name=$3
		AND ($4 = '' OR protocol=$4)
		RETURNING `+actionCols, f.WorkspaceID, f.RepoOwner, f.RepoName, f.Protocol, reason, l.now())
	if err != nil {
		return 0, err
	}
	var cancelled []Action
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			rows.Close()
			return 0, err
		}
		cancelled = append(cancelled, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, a := range cancelled {
		if err := l.releaseOwner(ctx, tx, a); err != nil {
			return 0, err
		}
	}
	return len(cancelled), tx.Commit(ctx)
}

// claimOwner makes the caller the one creator for (repo, signature)
// under an advisory lock held to the end of tx. false means an issue is being
// created, is open, or was closed inside reopenAfter: attach to it instead.
func (l *Ledger) claimOwner(ctx context.Context, tx pgx.Tx, ws, owner, repo, sig string, reopenAfter time.Duration) (bool, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, ownerKey(ws, owner, repo, sig)); err != nil {
		return false, err
	}
	var state string
	var closedAt *time.Time
	err := tx.QueryRow(ctx, `SELECT state, closed_at FROM ci_signature_owner WHERE workspace_id=$1::uuid
		AND repo_owner=$2 AND repo_name=$3 AND signature_hash=$4 FOR UPDATE`, ws, owner, repo, sig).Scan(&state, &closedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		_, err = tx.Exec(ctx, `INSERT INTO ci_signature_owner (workspace_id, repo_owner, repo_name, signature_hash, state, created_at)
			VALUES ($1::uuid,$2,$3,$4,'creating',$5)`, ws, owner, repo, sig, l.now())
		return err == nil, err
	case err != nil:
		return false, err
	case state != "closed" || (closedAt != nil && closedAt.Add(reopenAfter).After(l.now())):
		return false, nil
	}
	_, err = tx.Exec(ctx, `UPDATE ci_signature_owner SET state='creating', issue_id=NULL, closed_at=NULL
		WHERE workspace_id=$1::uuid AND repo_owner=$2 AND repo_name=$3 AND signature_hash=$4`, ws, owner, repo, sig)
	return err == nil, err
}
