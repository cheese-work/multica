package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/util"
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

	if outcome, err := s.refusal(ctx, tx, p); err != nil || outcome != "" {
		return RecoveryClaim{}, outcome, err
	}
	var claim RecoveryClaim
	if err := tx.QueryRow(ctx, `
		INSERT INTO recovery_claim (workspace_id, issue_id, failed_task_id, mode, claimed_at)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		p.WorkspaceID, p.IssueID, p.FailedTaskID, string(p.Mode), p.Now).Scan(&claim.ID); err != nil {
		return RecoveryClaim{}, "", fmt.Errorf("insert recovery claim: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return RecoveryClaim{}, "", fmt.Errorf("commit recovery claim: %w", err)
	}
	return claim, RecoveryAdmitted, nil
}

func (s *RecoveryClaimStore) refusal(ctx context.Context, tx pgx.Tx, p RecoveryClaimParams) (RecoveryClaimOutcome, error) {
	// Both native modes and fresh replay answer to the native ceiling: the
	// original attempt is attempt one, so retries stop at ceiling-1, and an
	// exhausted native lineage never earns a fresh replay.
	var retries int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM recovery_claim WHERE workspace_id = $1 AND issue_id = $2 AND mode IN ('native_resume', 'native_release') AND state <> 'released'`,
		p.WorkspaceID, p.IssueID).Scan(&retries); err != nil {
		return "", fmt.Errorf("count native recovery attempts: %w", err)
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
	return s.transition(ctx, id, `state = 'submitted', successor_task_id = $2`, `state = 'claimed'`, successorTaskID)
}

// MarkAmbiguous keeps the claim's slot until Settle reconciles it.
func (s *RecoveryClaimStore) MarkAmbiguous(ctx context.Context, id pgtype.UUID) error {
	return s.transition(ctx, id, `state = 'ambiguous'`, `state IN ('claimed', 'submitted')`)
}

// Settle ends a claim and frees its paced slot.
func (s *RecoveryClaimStore) Settle(ctx context.Context, id pgtype.UUID, state RecoveryState) error {
	return s.transition(ctx, id, `state = $2, settled_at = now()`, `state IN ('claimed', 'submitted', 'ambiguous')`, string(state))
}

func (s *RecoveryClaimStore) transition(ctx context.Context, id pgtype.UUID, set, from string, args ...any) error {
	tag, err := s.pool.Exec(ctx, `UPDATE recovery_claim SET `+set+` WHERE id = $1 AND `+from, append([]any{id}, args...)...)
	if err != nil {
		return fmt.Errorf("update recovery claim: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("recovery claim %s is not in a state that allows this transition", util.UUIDToString(id))
	}
	return nil
}
