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
}

// ErrNoWorkspace rejects a filter without a workspace: it would match every workspace.
var ErrNoWorkspace = errors.New("browserlease: revoke needs a workspace")

// ErrNoDecision rejects a decision revoke without a decision id.
var ErrNoDecision = errors.New("browserlease: revoke needs a decision")

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
	dropped, err := r.Queries.DeleteQueuedBrowserLeases(ctx, db.DeleteQueuedBrowserLeasesParams{
		WorkspaceID: f.WorkspaceID,
		LeaseID:     f.LeaseID,
		TaskID:      f.TaskID,
		UserID:      f.UserID,
		AgentID:     f.AgentID,
		ProfileID:   f.ProfileID,
		DecisionID:  f.DecisionID,
	})
	if err != nil {
		return nil, fmt.Errorf("drop queued browser leases: %w", err)
	}
	r.publish(dropped, reason)

	ended, err := r.Queries.EndBrowserLeases(ctx, db.EndBrowserLeasesParams{
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
		return dropped, fmt.Errorf("end browser leases: %w", err)
	}
	r.publish(ended, reason)
	return append(dropped, ended...), nil
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
