package caselifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var (
	ErrEvidenceExpired         = errors.New("governance case evidence is older than the freshness ceiling")
	ErrEvidenceFresh           = errors.New("governance case evidence has not expired")
	ErrRefreshUnreconciled     = errors.New("governance case has active or uncertain work to reconcile before refresh")
	ErrRefreshEvidenceRejected = errors.New("governance refresh evidence is not a rebuilt, complete, current capture")
)

// MaxEvidenceFreshness is the fixed freshness ceiling for case evidence,
// measured from the capture time of the evaluation. Callers may tighten it,
// never widen it.
const MaxEvidenceFreshness = 60 * time.Second

func effectiveFreshness(requested time.Duration) time.Duration {
	return min(requested, MaxEvidenceFreshness)
}

// RefreshCommand sweeps one correction_pending or human_review case whose evidence has aged
// out. The refresh work is keyed by (case, expired evidence epoch), so any
// number of sweeps or restarts resolve to the same transition rows.
type RefreshCommand struct {
	WorkspaceID      pgtype.UUID
	CaseID           pgtype.UUID
	ControlEpoch     int64
	ExpectedRevision int64
	MaxEvidenceAge   time.Duration
}

type RefreshResult struct {
	Case       db.GovernanceCase
	Transition db.GovernanceCaseTransition
	Duplicate  bool
	// Exhausted means the refresh limit was already consumed: the case went to
	// human review instead of refreshing.
	Exhausted bool
}

// CompleteRefreshCommand finishes a refresh with an evaluation that was
// captured after the expired one. A timestamp alone cannot complete it.
type CompleteRefreshCommand struct {
	WorkspaceID           pgtype.UUID
	CaseID                pgtype.UUID
	ControlEpoch          int64
	ExpectedRevision      int64
	ExpectedEvidenceEpoch int32
	FreshEvaluationID     pgtype.UUID
	MaxEvidenceAge        time.Duration
}

// databaseNow is the authoritative time for freshness and deadline guards: the
// database clock, read inside the active transaction after its locks. A read
// error fails the operation closed. The injected Clock only stamps rows.
func databaseNow(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("read governance database time: %w", err)
	}
	return now.UTC(), nil
}

func deadlinePassed(caseRow db.GovernanceCase, now time.Time) bool {
	return caseRow.AbsoluteDeadline.Valid && !now.Before(caseRow.AbsoluteDeadline.Time)
}

func refreshCauseKey(kind string, caseID pgtype.UUID, evidenceEpoch int32) string {
	return fmt.Sprintf("refresh-%s:%s:%d", kind, uuidKey(caseID), evidenceEpoch)
}

func (service *Service) BeginRefresh(ctx context.Context, command RefreshCommand) (RefreshResult, error) {
	if !validUUID(command.WorkspaceID) || !validUUID(command.CaseID) || command.ControlEpoch < 1 ||
		command.ExpectedRevision < 0 || command.MaxEvidenceAge <= 0 {
		return RefreshResult{}, ErrInvalidTransition
	}
	tx, err := service.database.Begin(ctx)
	if err != nil {
		return RefreshResult{}, fmt.Errorf("begin governance refresh: %w", err)
	}
	defer tx.Rollback(ctx)

	maxRefreshes, err := lockRefreshControl(ctx, tx, command.WorkspaceID, command.ControlEpoch)
	if err != nil {
		return RefreshResult{}, err
	}
	queries := db.New(tx)
	caseRow, err := queries.LockGovernanceCaseForUpdate(ctx, db.LockGovernanceCaseForUpdateParams{
		WorkspaceID: command.WorkspaceID, ID: command.CaseID,
	})
	if err != nil {
		return RefreshResult{}, fmt.Errorf("lock governance case for refresh: %w", err)
	}
	if caseRow.ControlEpoch != command.ControlEpoch {
		return RefreshResult{}, ErrStaleControlEpoch
	}
	expiryKey := refreshCauseKey("expiry", command.CaseID, caseRow.EvidenceEpoch)
	exhaustedKey := refreshCauseKey("exhausted", command.CaseID, caseRow.EvidenceEpoch)
	prior, err := queries.FindGovernanceCaseTransitionByCause(ctx, db.FindGovernanceCaseTransitionByCauseParams{
		WorkspaceID: command.WorkspaceID, CaseID: command.CaseID, CauseEventKey: expiryKey,
	})
	if err == nil {
		_, exhaustedErr := queries.FindGovernanceCaseTransitionByCause(ctx, db.FindGovernanceCaseTransitionByCauseParams{
			WorkspaceID: command.WorkspaceID, CaseID: command.CaseID, CauseEventKey: exhaustedKey,
		})
		if exhaustedErr != nil && !errors.Is(exhaustedErr, pgx.ErrNoRows) {
			return RefreshResult{}, fmt.Errorf("read governance refresh exhaustion: %w", exhaustedErr)
		}
		return RefreshResult{Case: caseRow, Transition: prior, Duplicate: true, Exhausted: exhaustedErr == nil}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return RefreshResult{}, fmt.Errorf("read governance refresh input: %w", err)
	}
	origin := CaseState(caseRow.State)
	if (origin != CaseCorrectionPending && origin != CaseHumanReview) || caseRow.StateRevision != command.ExpectedRevision {
		return RefreshResult{}, ErrStaleCase
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return RefreshResult{}, err
	}
	expiredID, capturedAt, err := currentEvidenceCapture(ctx, tx, caseRow)
	if err != nil {
		return RefreshResult{}, err
	}
	if now.Sub(capturedAt) <= effectiveFreshness(command.MaxEvidenceAge) {
		return RefreshResult{}, ErrEvidenceFresh
	}
	if err := requireReconciled(ctx, tx, caseRow); err != nil {
		return RefreshResult{}, err
	}
	if maxRefreshes == nil {
		return service.parkForMissingLimit(ctx, tx, queries, caseRow, expiryKey)
	}

	transition, err := service.transitionLocked(ctx, queries, caseRow, TransitionCommand{
		WorkspaceID: command.WorkspaceID, CaseID: command.CaseID, ControlEpoch: command.ControlEpoch,
		ExpectedState: origin, ExpectedRevision: command.ExpectedRevision, NextState: CaseRefreshing,
		CauseEventKey: expiryKey, Actor: ActorSystem, Reason: ReasonAgeOnlyExpiry,
	})
	if err != nil {
		return RefreshResult{}, err
	}
	result := RefreshResult{Case: transition.Case, Transition: transition.Transition}
	if int64(caseRow.RefreshCount) >= *maxRefreshes || deadlinePassed(caseRow, now) {
		exhausted, err := service.transitionLocked(ctx, queries, transition.Case, TransitionCommand{
			WorkspaceID: command.WorkspaceID, CaseID: command.CaseID, ControlEpoch: command.ControlEpoch,
			ExpectedState: CaseRefreshing, ExpectedRevision: command.ExpectedRevision + 1, NextState: CaseHumanReview,
			CauseEventKey: exhaustedKey, Actor: ActorSystem, Reason: ReasonRefreshBudgetExhausted,
		})
		if err != nil {
			return RefreshResult{}, err
		}
		result.Case, result.Transition, result.Exhausted = exhausted.Case, exhausted.Transition, true
	} else {
		result.Case, err = consumeRefresh(ctx, queries, tx, result.Case, expiredID)
		if err != nil {
			return RefreshResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return RefreshResult{}, fmt.Errorf("commit governance refresh: %w", err)
	}
	return result, nil
}

func (service *Service) CompleteRefresh(ctx context.Context, command CompleteRefreshCommand) (RefreshResult, error) {
	if !validUUID(command.WorkspaceID) || !validUUID(command.CaseID) || command.ControlEpoch < 1 ||
		command.ExpectedRevision < 0 || command.ExpectedEvidenceEpoch < 0 || command.MaxEvidenceAge <= 0 {
		return RefreshResult{}, ErrInvalidTransition
	}
	tx, err := service.database.Begin(ctx)
	if err != nil {
		return RefreshResult{}, fmt.Errorf("begin governance refresh completion: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := lockRefreshControl(ctx, tx, command.WorkspaceID, command.ControlEpoch); err != nil {
		return RefreshResult{}, err
	}
	queries := db.New(tx)
	caseRow, err := queries.LockGovernanceCaseForUpdate(ctx, db.LockGovernanceCaseForUpdateParams{
		WorkspaceID: command.WorkspaceID, ID: command.CaseID,
	})
	if err != nil {
		return RefreshResult{}, fmt.Errorf("lock governance case for refresh completion: %w", err)
	}
	if caseRow.ControlEpoch != command.ControlEpoch {
		return RefreshResult{}, ErrStaleControlEpoch
	}
	completeKey := refreshCauseKey("complete", command.CaseID, command.ExpectedEvidenceEpoch)
	prior, err := queries.FindGovernanceCaseTransitionByCause(ctx, db.FindGovernanceCaseTransitionByCauseParams{
		WorkspaceID: command.WorkspaceID, CaseID: command.CaseID, CauseEventKey: completeKey,
	})
	if err == nil {
		if caseRow.EvidenceID != command.FreshEvaluationID {
			return RefreshResult{}, ErrInputConflict
		}
		return RefreshResult{Case: caseRow, Transition: prior, Duplicate: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return RefreshResult{}, fmt.Errorf("read governance refresh completion: %w", err)
	}
	exhaustedKey := refreshCauseKey("exhausted", command.CaseID, command.ExpectedEvidenceEpoch)
	prior, err = queries.FindGovernanceCaseTransitionByCause(ctx, db.FindGovernanceCaseTransitionByCauseParams{
		WorkspaceID: command.WorkspaceID, CaseID: command.CaseID, CauseEventKey: exhaustedKey,
	})
	if err == nil {
		return RefreshResult{Case: caseRow, Transition: prior, Duplicate: true, Exhausted: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return RefreshResult{}, fmt.Errorf("read governance refresh exhaustion: %w", err)
	}
	if CaseState(caseRow.State) != CaseRefreshing || caseRow.StateRevision != command.ExpectedRevision {
		return RefreshResult{}, ErrStaleCase
	}
	if caseRow.EvidenceEpoch != command.ExpectedEvidenceEpoch {
		return RefreshResult{}, ErrStaleEvidenceEpoch
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return RefreshResult{}, err
	}
	if deadlinePassed(caseRow, now) {
		exhausted, err := service.transitionLocked(ctx, queries, caseRow, TransitionCommand{
			WorkspaceID: command.WorkspaceID, CaseID: command.CaseID, ControlEpoch: command.ControlEpoch,
			ExpectedState: CaseRefreshing, ExpectedRevision: command.ExpectedRevision, NextState: CaseHumanReview,
			CauseEventKey: exhaustedKey, Actor: ActorSystem, Reason: ReasonRefreshBudgetExhausted,
		})
		if err != nil {
			return RefreshResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return RefreshResult{}, fmt.Errorf("commit governance refresh deadline exhaustion: %w", err)
		}
		return RefreshResult{Case: exhausted.Case, Transition: exhausted.Transition, Exhausted: true}, nil
	}
	_, expiredAt, err := currentEvidenceCapture(ctx, tx, caseRow)
	if err != nil {
		return RefreshResult{}, err
	}
	fresh, err := readEvaluationCapture(ctx, tx, command.WorkspaceID, command.CaseID, command.FreshEvaluationID)
	if err != nil || !fresh.requiredComplete || fresh.redacted || command.FreshEvaluationID == caseRow.EvidenceID ||
		!fresh.capturedAt.After(expiredAt) || fresh.capturedAt.After(now) || now.Sub(fresh.capturedAt) > effectiveFreshness(command.MaxEvidenceAge) {
		return RefreshResult{}, ErrRefreshEvidenceRejected
	}

	next, reason := CaseEvidenceReady, ReasonFreshEvidence
	if fromHuman, err := hasHumanReviewHistory(ctx, tx, caseRow); err != nil {
		return RefreshResult{}, err
	} else if fromHuman {
		next, reason = CaseHumanReview, ReasonFreshEvidenceForHuman
	}
	transition, err := service.transitionLocked(ctx, queries, caseRow, TransitionCommand{
		WorkspaceID: command.WorkspaceID, CaseID: command.CaseID, ControlEpoch: command.ControlEpoch,
		ExpectedState: CaseRefreshing, ExpectedRevision: command.ExpectedRevision, NextState: next,
		CauseEventKey: completeKey, Actor: ActorSystem, Reason: reason,
	})
	if err != nil {
		return RefreshResult{}, err
	}
	updated, err := replaceEvidence(ctx, queries, tx, transition.Case, command.FreshEvaluationID, fresh.digest)
	if err != nil {
		return RefreshResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RefreshResult{}, fmt.Errorf("commit governance refresh completion: %w", err)
	}
	return RefreshResult{Case: updated, Transition: transition.Transition}, nil
}

func (service *Service) parkForMissingLimit(ctx context.Context, tx pgx.Tx, queries *db.Queries, caseRow db.GovernanceCase, causeKey string) (RefreshResult, error) {
	transition, err := service.transitionLocked(ctx, queries, caseRow, TransitionCommand{
		WorkspaceID: caseRow.WorkspaceID, CaseID: caseRow.ID, ControlEpoch: caseRow.ControlEpoch,
		ExpectedState: CaseCorrectionPending, ExpectedRevision: caseRow.StateRevision, NextState: CaseParked,
		CauseEventKey: causeKey, Actor: ActorSystem, Reason: ReasonConfigMissing,
	})
	if err != nil {
		return RefreshResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RefreshResult{}, fmt.Errorf("commit governance refresh park: %w", err)
	}
	return RefreshResult{Case: transition.Case, Transition: transition.Transition}, nil
}

// lockRefreshControl is lockGovernanceControl plus the configured refresh
// limit; a nil limit means it is not configured.
func lockRefreshControl(ctx context.Context, tx pgx.Tx, workspaceID pgtype.UUID, capturedControlEpoch int64) (*int64, error) {
	if capturedControlEpoch < 1 {
		return nil, ErrStaleControlEpoch
	}
	var controlEpoch int64
	var enabled bool
	var maxRefreshes *int64
	err := tx.QueryRow(ctx, `
		SELECT control_epoch, settings->>'jev_governance_enabled' = 'true',
		       CASE WHEN jsonb_typeof(settings->'limits'->'max_refreshes') = 'number'
		            THEN (settings->'limits'->>'max_refreshes')::bigint END
		FROM governance_workspace_config
		WHERE workspace_id = $1
		FOR SHARE
	`, workspaceID).Scan(&controlEpoch, &enabled, &maxRefreshes)
	if err != nil || !enabled || controlEpoch != capturedControlEpoch {
		return nil, ErrStaleControlEpoch
	}
	return maxRefreshes, nil
}

type evaluationCapture struct {
	capturedAt       time.Time
	digest           string
	requiredComplete bool
	redacted         bool
}

func readEvaluationCapture(ctx context.Context, tx pgx.Tx, workspaceID, caseID, evaluationID pgtype.UUID) (evaluationCapture, error) {
	var capture evaluationCapture
	err := tx.QueryRow(ctx, `
		SELECT captured_at, snapshot_digest, required_complete, redacted_at IS NOT NULL
		FROM governance_evaluation
		WHERE workspace_id = $1 AND case_id = $2 AND id = $3
	`, workspaceID, caseID, evaluationID).Scan(&capture.capturedAt, &capture.digest, &capture.requiredComplete, &capture.redacted)
	return capture, err
}

// currentEvidenceCapture is when the case's current evidence was captured: its
// bound evaluation, else the newest unredacted one.
func currentEvidenceCapture(ctx context.Context, tx pgx.Tx, caseRow db.GovernanceCase) (pgtype.UUID, time.Time, error) {
	var id pgtype.UUID
	var capturedAt time.Time
	err := tx.QueryRow(ctx, `
		SELECT id, captured_at FROM governance_evaluation
		WHERE workspace_id = $1 AND case_id = $2 AND redacted_at IS NULL AND ($3::uuid IS NULL OR id = $3)
		ORDER BY captured_at DESC, created_at DESC
		LIMIT 1
	`, caseRow.WorkspaceID, caseRow.ID, caseRow.EvidenceID).Scan(&id, &capturedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, time.Time{}, ErrRefreshEvidenceRejected
	}
	if err != nil {
		return pgtype.UUID{}, time.Time{}, fmt.Errorf("read governance evidence capture: %w", err)
	}
	return id, capturedAt.UTC(), nil
}

// requireReconciled blocks a refresh while any case sharing the retained
// budget root (the whole successor lineage) still owns an unfinished attempt,
// an in-flight action, or an unsettled budget reservation (active or uncertain
// outcome). Material succession must not launder an ancestor's liability.
func requireReconciled(ctx context.Context, tx pgx.Tx, caseRow db.GovernanceCase) error {
	var open bool
	err := tx.QueryRow(ctx, `
		WITH lineage AS (
			SELECT id, state, current_action_id FROM governance_case
			WHERE workspace_id = $1 AND budget_root_id = $2
		)
		SELECT EXISTS (SELECT 1 FROM lineage
		               WHERE current_action_id IS NOT NULL AND (id = $3 OR state NOT IN ('resolved', 'dismissed', 'abstained')))
		    OR EXISTS (SELECT 1 FROM governance_attempt
		               WHERE workspace_id = $1 AND case_id IN (SELECT id FROM lineage)
		                 AND terminal_at IS NULL AND redacted_at IS NULL)
		    OR EXISTS (SELECT 1 FROM governance_budget_reservation
		               WHERE workspace_id = $1 AND (budget_root_id = $2 OR case_id IN (SELECT id FROM lineage))
		                 AND state = 'reserved')
	`, caseRow.WorkspaceID, caseRow.BudgetRootID, caseRow.ID).Scan(&open)
	if err != nil {
		return fmt.Errorf("read governance unreconciled work: %w", err)
	}
	if open {
		return ErrRefreshUnreconciled
	}
	return nil
}

// hasHumanReviewHistory reports whether the case ever entered or left
// human_review. A prior human approval covers only the evidence epoch it
// reviewed, so any later refresh of such a case must return to human review.
func hasHumanReviewHistory(ctx context.Context, tx pgx.Tx, caseRow db.GovernanceCase) (bool, error) {
	var found bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM governance_case_transition
			WHERE workspace_id = $1 AND case_id = $2 AND (from_state = 'human_review' OR to_state = 'human_review')
		)
	`, caseRow.WorkspaceID, caseRow.ID).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("read governance human review history: %w", err)
	}
	return found, nil
}

// consumeRefresh counts the refresh and pins the expired evidence, so the
// replacement must be a different, later capture.
func consumeRefresh(ctx context.Context, queries *db.Queries, tx pgx.Tx, caseRow db.GovernanceCase, expiredID pgtype.UUID) (db.GovernanceCase, error) {
	if _, err := tx.Exec(ctx, `
		UPDATE governance_case SET refresh_count = refresh_count + 1, evidence_id = $4
		WHERE workspace_id = $1 AND id = $2 AND state_revision = $3
	`, caseRow.WorkspaceID, caseRow.ID, caseRow.StateRevision, expiredID); err != nil {
		return db.GovernanceCase{}, fmt.Errorf("consume governance refresh: %w", err)
	}
	return reloadCase(ctx, queries, caseRow)
}

func replaceEvidence(ctx context.Context, queries *db.Queries, tx pgx.Tx, caseRow db.GovernanceCase, evaluationID pgtype.UUID, digest string) (db.GovernanceCase, error) {
	if _, err := tx.Exec(ctx, `
		UPDATE governance_case SET evidence_epoch = evidence_epoch + 1, evidence_id = $4, evidence_digest = $5
		WHERE workspace_id = $1 AND id = $2 AND state_revision = $3
	`, caseRow.WorkspaceID, caseRow.ID, caseRow.StateRevision, evaluationID, digest); err != nil {
		return db.GovernanceCase{}, fmt.Errorf("replace governance evidence: %w", err)
	}
	return reloadCase(ctx, queries, caseRow)
}

func reloadCase(ctx context.Context, queries *db.Queries, caseRow db.GovernanceCase) (db.GovernanceCase, error) {
	reloaded, err := queries.LockGovernanceCaseForUpdate(ctx, db.LockGovernanceCaseForUpdateParams{
		WorkspaceID: caseRow.WorkspaceID, ID: caseRow.ID,
	})
	if err != nil {
		return db.GovernanceCase{}, fmt.Errorf("reload governance case: %w", err)
	}
	return reloaded, nil
}

func uuidKey(id pgtype.UUID) string {
	return fmt.Sprintf("%x", id.Bytes)
}
