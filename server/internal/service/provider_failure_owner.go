package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// providerFailureOwner names the one mechanism allowed to recover a failed task.
// `none` covers capped tasks, the recurring-trigger guard and tasks no recovery
// mechanism covers.
type providerFailureOwner string

const (
	providerOwnerNone        providerFailureOwner = "none"
	providerOwnerWakeupRearm providerFailureOwner = "wakeup_rearm"
	providerOwnerTaskRetry   providerFailureOwner = "task_retry"
)

// providerFailureOwnerDecision carries the rule that decided, so tests and
// notices can tell a cap (rules 1 and 2) from a guard-owned task (rule 3).
type providerFailureOwnerDecision struct {
	Owner providerFailureOwner
	Rule  int
}

// Capped reports the two rules after which the owner is told no recovery follows.
func (d providerFailureOwnerDecision) Capped() bool { return d.Rule == 1 || d.Rule == 2 }

// resolveProviderFailureOwner applies the owner rules in order (CHE-1732 M0b).
// A task that does not exist has no owner: rule 0, `none`.
func resolveProviderFailureOwner(ctx context.Context, q *db.Queries, failedTaskID pgtype.UUID) (providerFailureOwnerDecision, error) {
	facts, err := q.GetProviderFailureOwnerFacts(ctx, failedTaskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return providerFailureOwnerDecision{Owner: providerOwnerNone}, nil
	}
	if err != nil {
		return providerFailureOwnerDecision{}, fmt.Errorf("load provider failure owner facts: %w", err)
	}
	switch {
	case facts.TaskCap:
		return providerFailureOwnerDecision{providerOwnerNone, 1}, nil
	case facts.WakeupCap:
		return providerFailureOwnerDecision{providerOwnerNone, 2}, nil
	case facts.WakeupGuardOwned || facts.AutopilotGuardOwned:
		return providerFailureOwnerDecision{providerOwnerNone, 3}, nil
	case facts.HasWakeup && facts.WakeupOnce:
		return providerFailureOwnerDecision{providerOwnerWakeupRearm, 4}, nil
	case facts.HasWakeup:
		return providerFailureOwnerDecision{providerOwnerTaskRetry, 5}, nil
	case !facts.HasIssue || facts.InTriage || facts.NonScheduleAutopilot:
		return providerFailureOwnerDecision{providerOwnerNone, 6}, nil
	}
	return providerFailureOwnerDecision{providerOwnerTaskRetry, 7}, nil
}

// providerFailureEffectKind derives the effect from the new column shape and
// changed_condition, never from the legacy trigger_kind. The recurring-trigger
// guard's 401 effects, and any malformed shape, are `none`.
func providerFailureEffectKind(p db.RecordProviderFailureRecoveryParams) providerFailureOwner {
	if p.ChangedCondition != "recovery_reserved" {
		return providerOwnerNone
	}
	switch {
	case p.RecoveryWakeupID.Valid && !p.RecoveryTaskID.Valid && !p.RecoveryRunID.Valid:
		return providerOwnerWakeupRearm
	case p.RecoveryTaskID.Valid && !p.RecoveryWakeupID.Valid && !p.RecoveryRunID.Valid:
		return providerOwnerTaskRetry
	}
	return providerOwnerNone
}

// reserveProviderFailureRecoveryAndApply reserves and applies a recovery effect
// only when its kind equals the canonical owner of the failed task. A mismatch,
// a cap, a guard-owned or an unknown task inserts nothing and never calls apply;
// the caller's transaction stays usable.
func reserveProviderFailureRecoveryAndApply(ctx context.Context, tx pgx.Tx, reservation db.RecordProviderFailureRecoveryParams, apply func(context.Context, pgx.Tx) error) (bool, error) {
	if tx == nil {
		return false, errors.New("provider failure recovery reservation requires a transaction")
	}
	kind := providerFailureEffectKind(reservation)
	if kind == providerOwnerNone {
		return false, nil
	}
	decision, err := resolveProviderFailureOwner(ctx, db.New(tx), reservation.FailedTaskID)
	if err != nil {
		return false, err
	}
	if decision.Owner != kind {
		return false, nil
	}
	return applyProviderFailureReservation(ctx, tx, reservation, apply)
}

// notifyProviderFailureCap tells the failed run's accountable user, once, that
// a capped failure will not be retried. Only cap decisions notify; the other
// owner results are normal routing.
func (s *TaskService) notifyProviderFailureCap(ctx context.Context, q *db.Queries, failedTaskID pgtype.UUID, decision providerFailureOwnerDecision) {
	if s == nil || q == nil || !decision.Capped() || !failedTaskID.Valid {
		return
	}
	target, err := q.GetProviderFailureNoticeTarget(ctx, failedTaskID)
	if err != nil || !target.WorkspaceID.Valid || !target.AccountableUserID.Valid {
		slog.Warn("provider recovery cap notice skipped: no accountable user",
			"failed_task_id", util.UUIDToString(failedTaskID), "error", err)
		return
	}
	if _, err := q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: target.AccountableUserID, WorkspaceID: target.WorkspaceID}); err != nil {
		slog.Warn("provider recovery cap notice skipped: accountable user is not a workspace member",
			"failed_task_id", util.UUIDToString(failedTaskID), "error", err)
		return
	}
	details, err := json.Marshal(map[string]any{
		"reason_code":    "provider_failure_recovery_capped",
		"failed_task_id": util.UUIDToString(failedTaskID),
		"cap_rule":       decision.Rule,
	})
	if err != nil {
		return
	}
	rows, err := q.CreateProviderFailureCapNotice(ctx, db.CreateProviderFailureCapNoticeParams{
		ID:          failedTaskID,
		WorkspaceID: target.WorkspaceID,
		RecipientID: target.AccountableUserID,
		IssueID:     target.IssueID,
		Details:     details,
	})
	if err != nil {
		slog.Warn("provider recovery cap notice failed", "failed_task_id", util.UUIDToString(failedTaskID), "error", err)
		return
	}
	if rows == 1 {
		s.publishProviderFailureNotice(ctx, q, failedTaskID, target.WorkspaceID)
	}
}

// providerFailureTrigger maps a failed task to the legacy trigger columns of its
// recovery row. trigger_kind only admits autopilot_schedule (the guard's scope)
// and issue_wakeup, so an owner-routed issue task is always issue_wakeup: a task
// that came from a wakeup names that wakeup, any other issue task names itself.
// The columns are provenance only; no query matches a row by trigger_id.
func providerFailureTrigger(ctx context.Context, q *db.Queries, failedTaskID pgtype.UUID) (string, pgtype.UUID, error) {
	id, err := q.GetProviderFailureTriggerID(ctx, failedTaskID)
	if err != nil {
		return "", pgtype.UUID{}, fmt.Errorf("provider failure trigger: %w", err)
	}
	return "issue_wakeup", id, nil
}
