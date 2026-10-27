package handler

import (
	"context"
	"log/slog"
	"net/http"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

type wakeupActorKey struct{}
type wakeupActor struct{ kind, id, task string }
type wakeupHandoffKey struct{}

// wakeupHandoff names a write that hands an issue to an agent, and the agent.
type wakeupHandoff struct{ agent, write string }

// withWakeupHandedTo marks a write that hands the issue to agentID, whose run
// will deliver the change. The write's issue events are still captured for that
// agent, tagged with a write id (migration 666): the receipts are the agent's
// only copy until its run exists, so a stop between commit and enqueue loses
// nothing. consumeHandedWakeups settles them once the run is enqueued.
func withWakeupHandedTo(ctx context.Context, agentID string) (context.Context, wakeupHandoff) {
	if agentID == "" {
		return ctx, wakeupHandoff{}
	}
	handoff := wakeupHandoff{agent: agentID, write: dbid.NewV7().String()}
	return context.WithValue(ctx, wakeupHandoffKey{}, handoff), handoff
}

// consumeHandedWakeups marks the handed agent's issue-event receipts of this
// write handled, because the run it was handed now exists. A receipt merged
// with earlier pending events stays: it carries facts the run does not.
func (h *Handler) consumeHandedWakeups(ctx context.Context, issueID pgtype.UUID, handoff wakeupHandoff) {
	if handoff.agent == "" {
		return
	}
	err := h.Queries.ConsumeHandoffWakeupReceipts(ctx, db.ConsumeHandoffWakeupReceiptsParams{
		IssueID: issueID,
		AgentID: parseUUID(handoff.agent),
		Write:   handoff.write,
	})
	if err != nil {
		// The run exists, so a receipt left pending delivers the change again:
		// a duplicate, never a loss.
		slog.Warn("settle handed-to-agent wakeups failed", "issue_id", uuidToString(issueID), "agent_id", handoff.agent, "error", err)
	}
}

// Carry only server-resolved request identity into transaction-local settings.
// In particular, mutation of another agent's comment must not reuse its author
// or original run as the source of the new event.
func (h *Handler) withWakeupActor(r *http.Request) *http.Request {
	if _, ok := r.Context().Value(wakeupActorKey{}).(wakeupActor); ok {
		return r
	}
	kind, id := h.resolveActor(r, requestUserID(r), h.resolveWorkspaceID(r))
	source := h.wakeupSourceTaskID(r)
	var task string
	if source.Valid {
		task = uuidToString(source)
	}
	return r.WithContext(context.WithValue(r.Context(), wakeupActorKey{}, wakeupActor{kind, id, task}))
}

func (h *Handler) beginWakeupWrite(ctx context.Context) (pgx.Tx, error) {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if scope, ok := ctx.Value(projectWriteScopeKey{}).(projectWriteScope); ok {
		queries := h.Queries.WithTx(tx)
		if _, err := queries.LockWorkspaceForChatSessionCreate(ctx, scope.workspaceID); err != nil {
			_ = tx.Rollback(ctx)
			return nil, err
		}
		if err := queries.LockIssueStatusCatalogShared(ctx, scope.workspaceID); err != nil {
			_ = tx.Rollback(ctx)
			return nil, err
		}
		sort.Slice(scope.projectIDs, func(first, second int) bool {
			return uuidToString(scope.projectIDs[first]) < uuidToString(scope.projectIDs[second])
		})
		for _, projectID := range scope.projectIDs {
			if err := service.RequireActiveProject(ctx, queries, scope.workspaceID, projectID); err != nil {
				_ = tx.Rollback(ctx)
				return nil, err
			}
		}
		for _, observed := range scope.issues {
			current, err := queries.LockWakeupIssue(ctx, observed.ID)
			if err == nil && current.ProjectID != observed.ProjectID {
				err = errWorkflowChanged
			}
			if err != nil {
				_ = tx.Rollback(ctx)
				return nil, err
			}
		}
	}
	if actor, ok := ctx.Value(wakeupActorKey{}).(wakeupActor); ok {
		_, err = tx.Exec(ctx, `SELECT set_config('multica.actor_type',$1,true),set_config('multica.actor_id',$2,true),set_config('multica.source_task_id',$3,true)`, actor.kind, actor.id, actor.task)
		if err != nil {
			_ = tx.Rollback(ctx)
			return nil, err
		}
	}
	if handoff, ok := ctx.Value(wakeupHandoffKey{}).(wakeupHandoff); ok {
		if _, err = tx.Exec(ctx, `SELECT set_config('multica.handoff_write',$1,true)`, handoff.write); err != nil {
			_ = tx.Rollback(ctx)
			return nil, err
		}
	}
	return tx, nil
}

// Single-statement mutation paths use the same source attribution as existing
// multi-statement transactions. Settings cannot leak into the pooled connection.
func wakeupWrite[T any](h *Handler, r *http.Request, write func(*db.Queries) (T, error)) (T, error) {
	var zero T
	r = h.withWakeupActor(r)
	tx, err := h.beginWakeupWrite(r.Context())
	if err != nil {
		return zero, err
	}
	defer tx.Rollback(r.Context())
	result, err := write(h.Queries.WithTx(tx))
	if err != nil {
		return zero, err
	}
	if err := tx.Commit(r.Context()); err != nil {
		return zero, err
	}
	return result, nil
}
