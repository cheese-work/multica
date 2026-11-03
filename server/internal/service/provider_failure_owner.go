package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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
