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
	"github.com/multica-ai/multica/server/pkg/dbid"
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

// providerRecoveryActivityReserved is the timeline action of a recovery receipt.
const providerRecoveryActivityReserved = "provider_recovery_reserved"

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
//
// The returned decision tells the caller about a cap. Nothing is written or
// published here: when decision.Capped() the caller sends the notice with
// notifyProviderFailureCap after its own transaction outcome is final, never
// inside the transaction, because a rollback would lose the notice while its
// event was already published.
func reserveProviderFailureRecoveryAndApply(ctx context.Context, tx pgx.Tx, reservation db.RecordProviderFailureRecoveryParams, apply func(context.Context, pgx.Tx) error) (bool, providerFailureOwnerDecision, error) {
	if tx == nil {
		return false, providerFailureOwnerDecision{}, errors.New("provider failure recovery reservation requires a transaction")
	}
	kind := providerFailureEffectKind(reservation)
	if kind == providerOwnerNone {
		return false, providerFailureOwnerDecision{Owner: providerOwnerNone}, nil
	}
	decision, err := resolveProviderFailureOwner(ctx, db.New(tx), reservation.FailedTaskID)
	if err != nil {
		return false, providerFailureOwnerDecision{}, err
	}
	if decision.Owner != kind {
		return false, decision, nil
	}
	reserved, err := applyProviderFailureReservation(ctx, tx, reservation, func(ctx context.Context, sp pgx.Tx) error {
		if err := apply(ctx, sp); err != nil {
			return err
		}
		return writeProviderRecoveryReceipt(ctx, db.New(sp), reservation, kind)
	})
	return reserved, decision, err
}

// writeProviderRecoveryReceipt records the timeline receipt of one reserved
// recovery effect (CHE-1732 M0c). It runs inside the reservation savepoint, so
// it commits with the failure, the reservation and the effect and rolls back
// with them. A wakeup effect also records the wakeup's own `wakeup_rearmed`
// generation event. Both rows are inert data; nothing is published here.
func writeProviderRecoveryReceipt(ctx context.Context, q *db.Queries, reservation db.RecordProviderFailureRecoveryParams, kind providerFailureOwner) error {
	target, err := q.GetProviderFailureNoticeTarget(ctx, reservation.FailedTaskID)
	if err != nil {
		return fmt.Errorf("load provider recovery receipt target: %w", err)
	}
	failed := util.UUIDToString(reservation.FailedTaskID)
	details := map[string]any{"failed_task_id": failed, "effect_kind": string(kind), "reservation": failed}
	var wakeup db.IssueWakeup
	switch kind {
	case providerOwnerTaskRetry:
		details["effect_ref"] = util.UUIDToString(reservation.RecoveryTaskID)
	case providerOwnerWakeupRearm:
		wakeup, err = q.GetIssueWakeup(ctx, db.GetIssueWakeupParams{ID: reservation.RecoveryWakeupID, WorkspaceID: target.WorkspaceID})
		if err != nil {
			return fmt.Errorf("load re-armed wakeup: %w", err)
		}
		details["effect_ref"] = util.UUIDToString(reservation.RecoveryWakeupID)
		details["wakeup_revision"] = reservation.RecoveryWakeupRevision.Int64
	default:
		return fmt.Errorf("provider recovery receipt: unexpected effect kind %q", kind)
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return err
	}
	if _, err := q.CreateActivity(ctx, db.CreateActivityParams{
		ID: dbid.NewV7(), WorkspaceID: target.WorkspaceID, IssueID: target.IssueID,
		ActorType: pgtype.Text{String: "system", Valid: true}, Action: providerRecoveryActivityReserved, Details: raw,
	}); err != nil {
		return fmt.Errorf("record provider recovery receipt: %w", err)
	}
	if kind != providerOwnerWakeupRearm {
		return nil
	}
	_, err = recordWakeupActivity(ctx, q, wakeup, wakeupActivityRearmed, "system", pgtype.UUID{}, map[string]any{
		"failed_task_id": failed, "wakeup_revision": reservation.RecoveryWakeupRevision.Int64,
	})
	return err
}

// fenceRecoveryLineage reports whether a retryable failure sits in a provider
// failure recovery lineage (CHE-1773). A hit is a capped decision (task lineage
// or a chain deeper than the walk cap: rule 1; revision-aware wakeup lineage:
// rule 2) and means no generic retry. Every CreateRetryTask caller consults it.
func (s *TaskService) fenceRecoveryLineage(ctx context.Context, q *db.Queries, taskID pgtype.UUID) (providerFailureOwnerDecision, error) {
	facts, err := q.GetRecoveryLineageFacts(ctx, taskID)
	switch {
	case err != nil:
		return providerFailureOwnerDecision{}, err
	case facts.TaskLineage || facts.TooDeep:
		return providerFailureOwnerDecision{providerOwnerNone, 1}, nil
	case facts.WakeupLineage:
		return providerFailureOwnerDecision{providerOwnerNone, 2}, nil
	}
	return providerFailureOwnerDecision{}, nil
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

// notifyProviderFailureAttemptCap sends the cap notice a legacy reservation
// recorded on its attempt. Call it only after the caller's transaction is
// committed or rolled back, on the pool-backed queries.
func (s *TaskService) notifyProviderFailureAttemptCap(ctx context.Context, attempt *providerFailureRecoveryAttempt) {
	if s == nil || attempt == nil || attempt.Cap == nil {
		return
	}
	s.notifyProviderFailureCap(ctx, s.Queries, attempt.FailedTaskID, *attempt.Cap)
}
