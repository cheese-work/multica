package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

// CHE-1736 M1.2: a provider 5xx failure on an issue task gets one retry
// that is held (M3c context.provider_hold marker, status deferred), reserved
// through the M0b owner seam and receipted, all inside the failing transaction.
// Every refusal or failure of that effect leaves the failure committed.

// heldUnknownProvider is the marker provider for an agent whose model maps to no
// known provider. The row stays held until a release or max_hold_until.
const heldUnknownProvider = "unknown"

// errHeldRetryNotCreated makes the reservation savepoint roll back when the
// retry insert wrote no row, so no reservation or receipt outlives the miss.
var errHeldRetryNotCreated = errors.New("held provider retry: no row written")

// heldProviderRetry is the pre-transaction plan of one held retry.
type heldProviderRetry struct {
	engaged      bool
	reason       string
	maxAttempts  pgtype.Int4
	fireAt       pgtype.Timestamptz
	overlay      runtimeMCPOverlayData
	holdProvider pgtype.Text
	holdSeconds  pgtype.Int4
}

// isHeldProviderRetryReason names the one reason the held retry covers. 5xx is
// deliberately not in retryableReasons: retryShape and the held path read it, so
// failTask mints one successor. 429 stays on its generic retry until M3b
// (CHE-1741) can release a held row; the owner chose that in CHE-5284.
func isHeldProviderRetryReason(reason string) bool {
	return reason == taskfailure.ReasonAgentProviderServerError.String()
}

// planHeldProviderRetry loads, before the transaction, what the held retry
// needs: the agent, its MCP overlay (may do network I/O) and the hold marker
// values. It reports false when the task is outside the held path: other
// reasons, no issue, a wakeup-bound run (M1b) or no transaction handle.
func (s *TaskService) planHeldProviderRetry(ctx context.Context, reason string, parent db.AgentTaskQueue) (heldProviderRetry, bool) {
	if !isHeldProviderRetryReason(reason) || !parent.IssueID.Valid || s.TxStarter == nil || taskHasWakeupContext(parent) {
		return heldProviderRetry{}, false
	}
	agent, err := s.Queries.GetAgent(ctx, parent.AgentID)
	if err != nil {
		slog.Warn("held provider retry: load agent failed; no retry",
			"task_id", util.UUIDToString(parent.ID), "agent_id", util.UUIDToString(parent.AgentID), "error", err)
		return heldProviderRetry{engaged: true, reason: reason}, true
	}
	plan := heldProviderRetry{
		engaged:     true,
		reason:      reason,
		maxAttempts: pgtype.Int4{Int32: retryAttemptCeiling(reason, parent.MaxAttempts), Valid: true},
		overlay:     s.buildRuntimeMCPOverlay(ctx, parent.OriginatorUserID, agent),
	}
	plan.fireAt = pgtype.Timestamptz{Time: time.Now().Add(retryDelayForAttempt(reason, parent.Attempt)), Valid: true}
	plan.holdProvider, plan.holdSeconds, err = issueProviderHoldConfig(ctx, s.Queries, parent.AgentID)
	if err != nil {
		slog.Warn("held provider retry: hold config unreadable; using defaults",
			"task_id", util.UUIDToString(parent.ID), "error", err)
		plan.holdProvider, plan.holdSeconds = pgtype.Text{}, pgtype.Int4{}
	}
	if !plan.holdProvider.Valid {
		plan.holdProvider = pgtype.Text{String: heldUnknownProvider, Valid: true}
	}
	return plan, true
}

// taskHasWakeupContext reports a wakeup-bound run, whose recovery is M1b's.
func taskHasWakeupContext(t db.AgentTaskQueue) bool {
	if len(t.Context) == 0 {
		return false
	}
	var c struct {
		WakeupID string `json:"wakeup_id"`
	}
	return json.Unmarshal(t.Context, &c) == nil && c.WakeupID != ""
}

// heldRetryCreator inserts the retry row. failTask passes its own CreateRetryTask
// call, so the failing path keeps one insert site (TestEveryCreateRetryTaskCallSiteIsFenced).
type heldRetryCreator func(context.Context, *db.Queries, db.CreateRetryTaskParams) (db.AgentTaskQueue, error)

// createHeldProviderRetry runs inside the failing transaction after the failed
// status is written. It returns the held child when one was reserved and
// created. A nil child with a capped decision means the owner is told after
// commit. Every skip returns nil, nil: this transaction carries the failed
// status, so no refusal or effect failure may abort it.
func createHeldProviderRetry(ctx context.Context, qtx *db.Queries, tx pgx.Tx, failed db.AgentTaskQueue, plan heldProviderRetry, create heldRetryCreator) (*db.AgentTaskQueue, providerFailureOwnerDecision, error) {
	none := providerFailureOwnerDecision{Owner: providerOwnerNone}
	skip := func(why string, err error) (*db.AgentTaskQueue, providerFailureOwnerDecision, error) {
		slog.Info("held provider retry skipped: "+why,
			"task_id", util.UUIDToString(failed.ID), "issue_id", util.UUIDToString(failed.IssueID), "error", err)
		return nil, none, nil
	}
	if !plan.maxAttempts.Valid || tx == nil {
		return skip("not planned", nil)
	}
	triggerKind, triggerID, why, err := heldRetryPreChecks(ctx, tx, failed)
	if err != nil || why != "" {
		return skip(why, err)
	}
	failedAt := failed.CompletedAt
	if !failedAt.Valid {
		failedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	}
	childID := dbid.NewV7()
	var child db.AgentTaskQueue
	reserved, decision, err := reserveProviderFailureRecoveryAndApply(ctx, tx, db.RecordProviderFailureRecoveryParams{
		FailedTaskID:     failed.ID,
		TriggerKind:      triggerKind,
		TriggerID:        triggerID,
		FailedAt:         failedAt,
		ChangedCondition: "recovery_reserved",
		RecoveryTaskID:   childID,
	}, func(ctx context.Context, sp pgx.Tx) error {
		created, err := create(ctx, db.New(sp), db.CreateRetryTaskParams{
			NewTaskID:            childID,
			ID:                   failed.ID,
			FireAt:               plan.fireAt,
			MaxAttempts:          plan.maxAttempts,
			RuntimeMcpOverlay:    plan.overlay.Overlay,
			RuntimeConnectedApps: plan.overlay.ConnectedApps,
			HoldProvider:         plan.holdProvider,
			ProviderHoldSeconds:  plan.holdSeconds,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return errHeldRetryNotCreated
		}
		if err != nil {
			return fmt.Errorf("create held retry task: %w", err)
		}
		if err := noteRunInserted(ctx, db.New(sp), failed.IssueID, created.ID); err != nil {
			return err
		}
		if err := db.New(sp).CopyChannelTaskDelivery(ctx, db.CopyChannelTaskDeliveryParams{ChildTaskID: created.ID, ParentTaskID: failed.ID}); err != nil {
			return fmt.Errorf("copy held retry channel delivery: %w", err)
		}
		child = created
		return nil
	})
	switch {
	case err != nil:
		return skip("effect failed", err)
	case !reserved:
		return nil, decision, nil
	}
	return &child, decision, nil
}

// heldRetryPreChecks runs the refusals in a savepoint: a server-side SQL error
// there must not abort the failing transaction, which carries the failed
// status. A refusal or error rolls the savepoint back. When the retry may go
// ahead (why is empty) the savepoint is released instead, so the run-budget
// lock that guardIssueRunAllowed takes stays held until noteRunInserted debits
// the run.
func heldRetryPreChecks(ctx context.Context, tx pgx.Tx, failed db.AgentTaskQueue) (kind string, id pgtype.UUID, why string, err error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return "", pgtype.UUID{}, "pre-check savepoint failed", err
	}
	released := false
	defer func() {
		if !released {
			_ = sp.Rollback(ctx)
		}
	}()
	q := db.New(sp)
	if err := requireActiveTaskProject(ctx, q, failed); err != nil {
		return "", pgtype.UUID{}, "project is not active", err
	}
	if successor, err := hasRunnableSuccessor(ctx, q, failed); err != nil || successor {
		return "", pgtype.UUID{}, "a successor is already pending or unreadable", err
	}
	if err := guardIssueRunAllowed(ctx, q, failed.IssueID, OriginDerived, RunTarget{AgentID: failed.AgentID, Trigger: "retry"}); err != nil {
		return "", pgtype.UUID{}, "issue does not run", err
	}
	if allowed, err := q.GetProviderHeldRetryFence(ctx, failed.ID); err != nil || !allowed {
		return "", pgtype.UUID{}, "newer run, terminal issue or reassignment", err
	}
	kind, id, err = providerFailureTrigger(ctx, q, failed.ID)
	if err != nil {
		return "", pgtype.UUID{}, "trigger unreadable", err
	}
	if err := sp.Commit(ctx); err != nil {
		return "", pgtype.UUID{}, "pre-check savepoint release failed", err
	}
	released = true
	return kind, id, "", nil
}
