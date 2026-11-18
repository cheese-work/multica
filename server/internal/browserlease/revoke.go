package browserlease

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Revoke reasons, stored in browser_lease.end_reason and sent in the event.
const (
	ReasonRelease           = "release"
	ReasonTaskEnd           = "task_end"
	ReasonTaskCancelled     = "task_cancelled"
	ReasonPermissionRemoved = "permission_removed"
	ReasonPluginDisabled    = "plugin_disabled"
	ReasonRevokeAll         = "revoke_all"
	ReasonDecisionWithdrawn = "decision_withdrawn"
	ReasonDecisionExpired   = "decision_expired"
)

// Filter picks the leases to revoke. WorkspaceID is required. Any other valid field narrows the
// match; an all-empty filter revokes the whole workspace.
type Filter struct {
	WorkspaceID pgtype.UUID
	LeaseID     pgtype.UUID
	TaskID      pgtype.UUID
	UserID      pgtype.UUID
	AgentID     pgtype.UUID
	ProfileID   pgtype.UUID
	DecisionID  pgtype.UUID
}

// Revoker ends leases and tells the gateway at once.
type Revoker struct {
	Queries *db.Queries
	Bus     *events.Bus
	// Pool and Audit record each revoke inside the transaction that ends the leases
	// (CHE-4744). Audit needs Pool.
	Pool  TxBeginner
	Audit AuditFunc
}

// ErrNoWorkspace rejects a filter without a workspace: it would match every workspace.
var ErrNoWorkspace = errors.New("browserlease: revoke needs a workspace")

// Revoke ends every queued or active lease that matches f and publishes one revoke event per
// lease before it returns. A lease that already ended is left alone and emits nothing.
//
// Queued leases go first: they hold nothing, so they are deleted. Active leases are ended
// second, so a grant that races the delete is still caught. Each step publishes its events
// before the next one starts, so a failure in step 2 never hides step 1's events.
func (r *Revoker) Revoke(ctx context.Context, f Filter, reason string) ([]db.BrowserLease, error) {
	if !f.WorkspaceID.Valid {
		return nil, ErrNoWorkspace
	}
	var dropped []db.BrowserLease
	err := r.inTx(ctx, func(q *db.Queries) error {
		rows, err := q.DeleteQueuedBrowserLeases(ctx, db.DeleteQueuedBrowserLeasesParams{
			WorkspaceID: f.WorkspaceID,
			LeaseID:     f.LeaseID,
			TaskID:      f.TaskID,
			UserID:      f.UserID,
			AgentID:     f.AgentID,
			ProfileID:   f.ProfileID,
			DecisionID:  f.DecisionID,
		})
		if err != nil {
			return fmt.Errorf("drop queued browser leases: %w", err)
		}
		for _, l := range rows {
			// A queued lease never started or ended; the revoke reason is all it has to say.
			l.EndReason = pgtype.Text{String: reason, Valid: true}
			if err := r.audit(ctx, q, l); err != nil {
				return err
			}
		}
		dropped = rows
		return nil
	})
	if err != nil {
		return nil, err
	}
	r.publish(dropped, reason)

	var ended []db.BrowserLease
	err = r.inTx(ctx, func(q *db.Queries) error {
		rows, err := q.EndBrowserLeases(ctx, db.EndBrowserLeasesParams{
			Reason:      reason,
			WorkspaceID: f.WorkspaceID,
			LeaseID:     f.LeaseID,
			TaskID:      f.TaskID,
			UserID:      f.UserID,
			AgentID:     f.AgentID,
			ProfileID:   f.ProfileID,
			DecisionID:  f.DecisionID,
		})
		if err != nil {
			return fmt.Errorf("end browser leases: %w", err)
		}
		for _, l := range rows {
			if err := r.audit(ctx, q, l); err != nil {
				return err
			}
		}
		ended = rows
		return nil
	})
	if err != nil {
		return dropped, err
	}
	r.publish(ended, reason)
	return append(dropped, ended...), nil
}

// ErrAuditNeedsPool rejects a Revoker that must audit but cannot open the transaction that
// keeps the audit row and the revoke together.
var ErrAuditNeedsPool = errors.New("browserlease: a Revoker with Audit needs a Pool")

// inTx runs fn in one transaction when the Revoker audits, so the revoke and its audit rows
// commit or roll back together. Without Audit it runs fn on the plain queries.
func (r *Revoker) inTx(ctx context.Context, fn func(q *db.Queries) error) error {
	if r.Audit == nil {
		return fn(r.Queries)
	}
	if r.Pool == nil {
		return ErrAuditNeedsPool
	}
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(r.Queries.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *Revoker) audit(ctx context.Context, q *db.Queries, l db.BrowserLease) error {
	if r.Audit == nil {
		return nil
	}
	return r.Audit(ctx, q, AuditRevoke, l)
}

func (r *Revoker) publish(leases []db.BrowserLease, reason string) {
	for _, l := range leases {
		r.Bus.Publish(events.Event{
			Type:        protocol.EventBrowserLeaseRevoked,
			WorkspaceID: util.UUIDToString(l.WorkspaceID),
			ActorType:   "system",
			TaskID:      util.UUIDToString(l.TaskID),
			Payload: map[string]any{
				"lease_id":    util.UUIDToString(l.ID),
				"instance_id": util.UUIDToString(l.InstanceID),
				"task_id":     util.UUIDToString(l.TaskID),
				"epoch":       l.Epoch.Int64, // 0 for a queued lease: it never held the instance
				"reason":      reason,
			},
		})
	}
}

// Register revokes a task's leases when the task completes, fails or is cancelled.
func (r *Revoker) Register() {
	for event, reason := range map[string]string{
		protocol.EventTaskCompleted: ReasonTaskEnd,
		protocol.EventTaskFailed:    ReasonTaskEnd,
		protocol.EventTaskCancelled: ReasonTaskCancelled,
	} {
		r.Bus.Subscribe(event, func(e events.Event) { r.onTaskEnd(e, reason) })
	}
}

func (r *Revoker) onTaskEnd(e events.Event, reason string) {
	workspaceID, err := util.ParseUUID(e.WorkspaceID)
	if err != nil {
		return
	}
	taskID, err := util.ParseUUID(e.TaskID)
	if err != nil {
		return
	}
	// A bus handler has no caller to return to: log, and rely on the lease's own expiry as the
	// backstop (hard limit 2 h, token 15 min).
	if _, err := r.Revoke(context.Background(), Filter{WorkspaceID: workspaceID, TaskID: taskID}, reason); err != nil {
		slog.Error("browser lease: revoke on task end failed", "task_id", e.TaskID, "reason", reason, "error", err)
	}
}

// ErrNoDecision rejects a decision revoke without a decision id: the empty filter would match
// every lease of the workspace.
var ErrNoDecision = errors.New("browserlease: revoke by decision needs a decision id")

// RevokeDecision ends every queued or active lease bound to the decision (CHE-4742, W3-13).
// Call it once the decision is withdrawn or has expired, with ReasonDecisionWithdrawn or
// ReasonDecisionExpired. Leases bound to another decision, or to none, stay live.
func (r *Revoker) RevokeDecision(ctx context.Context, workspaceID, decisionID pgtype.UUID, reason string) ([]db.BrowserLease, error) {
	if !decisionID.Valid {
		return nil, ErrNoDecision
	}
	return r.Revoke(ctx, Filter{WorkspaceID: workspaceID, DecisionID: decisionID}, reason)
}
