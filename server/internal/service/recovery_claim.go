package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type RecoveryMode string

const (
	RecoveryModeFreshReplay   RecoveryMode = "fresh_replay"
	RecoveryModeNativeResume  RecoveryMode = "native_resume"
	RecoveryModeNativeRelease RecoveryMode = "native_release"
)

type RecoveryState string

const (
	RecoveryStateCompleted  RecoveryState = "completed"
	RecoveryStateFailed     RecoveryState = "failed"
	RecoveryStateReconciled RecoveryState = "reconciled"
	RecoveryStateReleased   RecoveryState = "released"
)

type RecoveryClaimOutcome string

const (
	RecoveryAdmitted            RecoveryClaimOutcome = "admitted"
	RecoveryDuplicate           RecoveryClaimOutcome = "duplicate"
	RecoveryIssueAllowanceSpent RecoveryClaimOutcome = "issue_allowance_spent"
	RecoveryNativeCeiling       RecoveryClaimOutcome = "native_ceiling"
	RecoveryWaitCap             RecoveryClaimOutcome = "wait_cap"
	RecoveryWaitSpacing         RecoveryClaimOutcome = "wait_spacing"
)

type RecoveryClaim struct {
	ID pgtype.UUID
}

type RecoveryClaimParams struct {
	WorkspaceID, IssueID, FailedTaskID pgtype.UUID
	Mode                               RecoveryMode
	NativeCeiling, PacedCap            int
	Spacing                            time.Duration
	Now                                time.Time
}

type RecoveryClaimStore struct{ pool *pgxpool.Pool }

func NewRecoveryClaimStore(pool *pgxpool.Pool) *RecoveryClaimStore {
	return &RecoveryClaimStore{pool: pool}
}

// Claim admits at most one recovery per failed run. Admission is serialized
// per workspace so the paced cap and spacing hold across concurrent callers.
// Native resume retries are recorded and bounded by the native ceiling but are
// exempt from the paced cap and spacing.
func (s *RecoveryClaimStore) Claim(ctx context.Context, p RecoveryClaimParams) (RecoveryClaim, RecoveryClaimOutcome, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RecoveryClaim{}, "", fmt.Errorf("begin recovery claim: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('recovery_claim:' || $1::text, 0))`, p.WorkspaceID); err != nil {
		return RecoveryClaim{}, "", fmt.Errorf("lock recovery claims: %w", err)
	}
	var existing RecoveryClaim
	err = tx.QueryRow(ctx, `SELECT id FROM recovery_claim WHERE failed_task_id = $1`, p.FailedTaskID).Scan(&existing.ID)
	if err == nil {
		return existing, RecoveryDuplicate, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return RecoveryClaim{}, "", fmt.Errorf("read existing recovery claim: %w", err)
	}

	root, err := lineageRoot(ctx, tx, p.FailedTaskID)
	if err != nil {
		return RecoveryClaim{}, "", err
	}
	if outcome, err := s.refusal(ctx, tx, p, root); err != nil || outcome != "" {
		return RecoveryClaim{}, outcome, err
	}
	var claim RecoveryClaim
	if err := tx.QueryRow(ctx, `
		INSERT INTO recovery_claim (workspace_id, issue_id, failed_task_id, lineage_root_task_id, mode, claimed_at)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		p.WorkspaceID, p.IssueID, p.FailedTaskID, root, string(p.Mode), p.Now).Scan(&claim.ID); err != nil {
		return RecoveryClaim{}, "", fmt.Errorf("insert recovery claim: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return RecoveryClaim{}, "", fmt.Errorf("commit recovery claim: %w", err)
	}
	return claim, RecoveryAdmitted, nil
}

// lineageRoot returns the top of the failed run's ancestry, or the run itself
// when it has no recorded task row. A native retry points at its parent via
// parent_task_id and a fresh replay at its source via rerun_of_task_id; both
// are retries of one lineage. The depth bound stops a corrupt cycle from
// recursing forever.
func lineageRoot(ctx context.Context, tx pgx.Tx, failedTaskID pgtype.UUID) (pgtype.UUID, error) {
	var root pgtype.UUID
	err := tx.QueryRow(ctx, `
		WITH RECURSIVE chain(id, parent_task_id, depth) AS (
			SELECT id, COALESCE(parent_task_id, rerun_of_task_id), 0 FROM agent_task_queue WHERE id = $1
			UNION ALL
			SELECT q.id, COALESCE(q.parent_task_id, q.rerun_of_task_id), chain.depth + 1
			FROM agent_task_queue q JOIN chain ON q.id = chain.parent_task_id
			WHERE chain.depth < 1000
		)
		SELECT id FROM chain ORDER BY depth DESC LIMIT 1`, failedTaskID).Scan(&root)
	if errors.Is(err, pgx.ErrNoRows) {
		return failedTaskID, nil
	}
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("resolve recovery lineage: %w", err)
	}
	return root, nil
}

func (s *RecoveryClaimStore) refusal(ctx context.Context, tx pgx.Tx, p RecoveryClaimParams, root pgtype.UUID) (RecoveryClaimOutcome, error) {
	// Every automatic retry of the lineage, native or replay, spends one budget:
	// the original attempt is attempt one, so retries stop at ceiling-1, and an
	// exhausted lineage never earns another retry from any writer.
	var retries int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM recovery_claim WHERE workspace_id = $1 AND issue_id = $2 AND lineage_root_task_id = $3 AND state <> 'released'`,
		p.WorkspaceID, p.IssueID, root).Scan(&retries); err != nil {
		return "", fmt.Errorf("count recovery attempts: %w", err)
	}
	if retries >= p.NativeCeiling-1 {
		return RecoveryNativeCeiling, nil
	}
	if p.Mode == RecoveryModeNativeResume {
		return "", nil
	}
	if p.Mode == RecoveryModeFreshReplay {
		var spent bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM recovery_claim WHERE workspace_id = $1 AND issue_id = $2 AND mode = 'fresh_replay' AND state <> 'released')`,
			p.WorkspaceID, p.IssueID).Scan(&spent); err != nil {
			return "", fmt.Errorf("read fresh replay allowance: %w", err)
		}
		if spent {
			return RecoveryIssueAllowanceSpent, nil
		}
	}
	var inFlight int
	var lastClaimedAt pgtype.Timestamptz
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE state IN ('claimed', 'submitted', 'ambiguous')), max(claimed_at)
		FROM recovery_claim
		WHERE workspace_id = $1 AND mode <> 'native_resume' AND state <> 'released'`,
		p.WorkspaceID).Scan(&inFlight, &lastClaimedAt); err != nil {
		return "", fmt.Errorf("read paced recovery load: %w", err)
	}
	if inFlight >= p.PacedCap {
		return RecoveryWaitCap, nil
	}
	if lastClaimedAt.Valid && p.Now.Sub(lastClaimedAt.Time) < p.Spacing {
		return RecoveryWaitSpacing, nil
	}
	return "", nil
}

// MarkSubmitted records the successor task of a claim whose submission is known.
func (s *RecoveryClaimStore) MarkSubmitted(ctx context.Context, id, successorTaskID pgtype.UUID) error {
	return s.MarkSubmittedIn(ctx, s.pool, id, successorTaskID)
}

// MarkSubmittedIn is MarkSubmitted on the caller's executor. Inside the
// dispatch transaction it commits with the dispatch and rolls back with it, so
// a claim is never submitted for a task that was not minted (CHE-4568).
func (s *RecoveryClaimStore) MarkSubmittedIn(ctx context.Context, ex db.DBTX, id, successorTaskID pgtype.UUID) error {
	return transition(ctx, ex, id, `state = 'submitted', successor_task_id = $2`, `state = 'claimed'`, successorTaskID)
}

// MarkAmbiguous keeps the claim's slot until Settle reconciles it.
func (s *RecoveryClaimStore) MarkAmbiguous(ctx context.Context, id pgtype.UUID) error {
	return transition(ctx, s.pool, id, `state = 'ambiguous'`, `state IN ('claimed', 'submitted')`)
}

// Settle ends a claim and frees its paced slot.
func (s *RecoveryClaimStore) Settle(ctx context.Context, id pgtype.UUID, state RecoveryState) error {
	return transition(ctx, s.pool, id, `state = $2, settled_at = now()`, `state IN ('claimed', 'submitted', 'ambiguous')`, string(state))
}

func transition(ctx context.Context, ex db.DBTX, id pgtype.UUID, set, from string, args ...any) error {
	tag, err := ex.Exec(ctx, `UPDATE recovery_claim SET `+set+` WHERE id = $1 AND `+from, append([]any{id}, args...)...)
	if err != nil {
		return fmt.Errorf("update recovery claim: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("recovery claim %s is not in a state that allows this transition", util.UUIDToString(id))
	}
	return nil
}

// Policy shared by every automatic retry writer, so one failed lineage spends
// one budget whichever writer asks (CHE-4568).
const (
	recoveryPacedCap   = 2
	recoverySpacing    = 2 * time.Minute
	recoveryStaleClaim = 5 * time.Minute
)

// ErrRecoveryNotAdmitted: the claim store refused a recovery dispatch.
var ErrRecoveryNotAdmitted = errors.New("recovery claim not admitted")

// Discard drops a claim that never dispatched anything, so the failed run can
// still be recovered later. Released claims stay: they record a decision.
func (s *RecoveryClaimStore) Discard(ctx context.Context, id pgtype.UUID) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM recovery_claim WHERE id = $1 AND state = 'claimed'`, id)
	if err != nil {
		return fmt.Errorf("discard recovery claim: %w", err)
	}
	return nil
}

// dispatchedTask is the task a stale claim's writer dispatched, found from
// evidence rather than from the claim's own state: the recorded successor, a
// task derived from the failed run, or the autopilot run a provider-failure
// reservation anchored. NULL means nothing was dispatched.
// ponytail: per stale claim, not per request; index the derived columns if
// unsettled claims ever pile up.
const dispatchedTask = `(SELECT q.id FROM agent_task_queue q
	WHERE q.id = c.successor_task_id OR q.parent_task_id = c.failed_task_id
	   OR q.retry_of_task_id = c.failed_task_id OR q.rerun_of_task_id = c.failed_task_id
	   OR q.autopilot_run_id IN (SELECT r.recovery_run_id FROM provider_failure_recovery r WHERE r.failed_task_id = c.failed_task_id)
	ORDER BY (q.id = c.successor_task_id) DESC, q.created_at LIMIT 1)`

// ReapSettled settles claims nobody else will, from what the database shows.
// A stale claim whose dispatch committed (crash before MarkSubmitted, or an
// autopilot task minted after the reservation) is bound to that task and keeps
// its slot until the task ends. A stale claim with no dispatch and no
// reservation never reached the queue, so it is removed and the failed run
// becomes recoverable again. A reservation with no task yet stays ambiguous,
// which holds its slot. Without this the paced cap saturates forever.
func (s *RecoveryClaimStore) ReapSettled(ctx context.Context, workspaceID pgtype.UUID, now time.Time) error {
	stale := now.Add(-recoveryStaleClaim)
	for _, stmt := range []string{
		`UPDATE recovery_claim c SET state = 'submitted', successor_task_id = ` + dispatchedTask + `
		 WHERE c.workspace_id = $1 AND c.claimed_at < $2 AND c.successor_task_id IS NULL AND c.state IN ('claimed', 'submitted', 'ambiguous')
		   AND ` + dispatchedTask + ` IS NOT NULL`,
		`UPDATE recovery_claim c SET state = CASE q.status WHEN 'completed' THEN 'completed' ELSE 'failed' END, settled_at = now()
		 FROM agent_task_queue q
		 WHERE c.workspace_id = $1 AND c.state = 'submitted' AND c.claimed_at < $2 AND q.id = c.successor_task_id AND q.status IN ('completed', 'failed', 'cancelled')`,
		`DELETE FROM recovery_claim c
		 WHERE c.workspace_id = $1 AND c.claimed_at < $2 AND c.state IN ('claimed', 'submitted') AND ` + dispatchedTask + ` IS NULL
		   AND NOT EXISTS (SELECT 1 FROM provider_failure_recovery r WHERE r.failed_task_id = c.failed_task_id)`,
		`UPDATE recovery_claim c SET state = 'ambiguous'
		 WHERE c.workspace_id = $1 AND c.claimed_at < $2 AND c.state IN ('claimed', 'submitted') AND ` + dispatchedTask + ` IS NULL`,
	} {
		if _, err := s.pool.Exec(ctx, stmt, workspaceID, stale); err != nil {
			return fmt.Errorf("reap recovery claims: %w", err)
		}
	}
	return nil
}

// recoveryHold carries an admitted claim from a writer's gate to its outcome.
type recoveryHold struct {
	claim     RecoveryClaim
	held      bool
	submitted bool // marked inside the dispatch transaction
}

// claimRecovery asks the shared store to admit one recovery dispatch for the
// failed run. admitted=false with a nil error is a refusal; the caller must
// not dispatch. A nil store admits everything (isolated test services), and so
// does a run with no issue to anchor the claim to.
func (s *TaskService) claimRecovery(ctx context.Context, hold *recoveryHold, issueID, failedTaskID pgtype.UUID, mode RecoveryMode, ceiling int) (bool, error) {
	if s.RecoveryClaims == nil || !issueID.Valid {
		return true, nil
	}
	issue, err := s.Queries.GetIssue(ctx, issueID)
	if err != nil {
		return false, fmt.Errorf("recovery claim: load issue: %w", err)
	}
	now := time.Now()
	if mode != RecoveryModeNativeResume {
		if err := s.RecoveryClaims.ReapSettled(ctx, issue.WorkspaceID, now); err != nil {
			slog.Warn("recovery claim: reap failed", "error", err)
		}
	}
	claim, outcome, err := s.RecoveryClaims.Claim(ctx, RecoveryClaimParams{
		WorkspaceID: issue.WorkspaceID, IssueID: issueID, FailedTaskID: failedTaskID, Mode: mode,
		NativeCeiling: ceiling, PacedCap: recoveryPacedCap, Spacing: recoverySpacing, Now: now,
	})
	if err != nil {
		return false, err
	}
	if outcome != RecoveryAdmitted {
		slog.Info("recovery claim refused", "failed_task_id", util.UUIDToString(failedTaskID), "mode", string(mode), "outcome", string(outcome))
		return false, nil
	}
	hold.claim, hold.held = claim, true
	return true, nil
}

// submitRecoveryClaim marks the claim submitted on the dispatch transaction's
// own executor, so it commits or rolls back with the dispatch. A failure is
// logged, not returned: the reaper resolves the claim from the queue.
func (s *TaskService) submitRecoveryClaim(ctx context.Context, hold *recoveryHold, q *db.Queries, successorTaskID pgtype.UUID) {
	if hold == nil || !hold.held {
		return
	}
	if err := s.RecoveryClaims.MarkSubmittedIn(ctx, q.Conn(), hold.claim.ID, successorTaskID); err != nil {
		slog.Warn("recovery claim: submit failed", "claim_id", util.UUIDToString(hold.claim.ID), "error", err)
		return
	}
	hold.submitted = true
}

// finishRecoveryClaim records what the writer did with its claim: a failed
// write discards it (nothing was dispatched, the run stays recoverable), a
// dispatch outside a transaction marks it submitted, and a decision not to
// dispatch releases it. A claim submitted inside the transaction is already
// final once the write succeeded.
// ponytail: a RerunIssue crash between its enqueue and this call leaves a
// claimed row; ReapSettled binds it to the dispatched task.
func (s *TaskService) finishRecoveryClaim(ctx context.Context, hold *recoveryHold, successorTaskID pgtype.UUID, dispatched bool, writeErr error) {
	if hold == nil || !hold.held || hold.submitted && writeErr == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	var err error
	switch {
	case writeErr != nil:
		err = s.RecoveryClaims.Discard(ctx, hold.claim.ID)
	case dispatched:
		err = s.RecoveryClaims.MarkSubmitted(ctx, hold.claim.ID, successorTaskID)
	default:
		err = s.RecoveryClaims.Settle(ctx, hold.claim.ID, RecoveryStateReleased)
	}
	if err != nil {
		slog.Warn("recovery claim: finish failed", "claim_id", util.UUIDToString(hold.claim.ID), "error", err)
	}
}
