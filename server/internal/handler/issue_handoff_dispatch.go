package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// CHE-5486 (S1b): a workflow handoff to an agent or squad is written as an
// intent row in the status transaction. dispatchHandoffIntent turns the intent
// into exactly one run: the run carries the intent's id (a unique index on
// agent_task_queue.handoff_dispatch_id), so a second dispatch finds the run in
// whatever state it has reached and creates no other.

const (
	handoffPending    = "pending"
	handoffDispatched = "dispatched"
	handoffSkipped    = "skipped"
)

type handoffIntentKey struct{}

// handoffIntentSeed is what the status transaction needs to write the intent.
type handoffIntentSeed struct {
	actorType, actorID string
	sourceTask         pgtype.UUID
	commentIDs         []pgtype.UUID
	write              string
}

// armHandoffIntent asks the status write in ctx's record to write a handoff
// intent. A legacy handoff_note travels with the run it starts, so it keeps the
// direct path.
func armHandoffIntent(rec *statusRecord, r *http.Request, actorType, actorID, handoffCommentID, handoffNote, write string) {
	if rec == nil || write == "" || handoffNote != "" {
		return
	}
	// Empty, not nil: a nil slice is NULL and the column is NOT NULL.
	seed := &handoffIntentSeed{actorType: actorType, actorID: actorID, write: write, commentIDs: []pgtype.UUID{}}
	if id, err := util.ParseUUID(r.Header.Get("X-Task-ID")); err == nil {
		seed.sourceTask = id
	}
	if id, err := util.ParseUUID(handoffCommentID); err == nil {
		seed.commentIDs = []pgtype.UUID{id}
	}
	rec.intent = seed
}

// writeHandoffIntent records the intent for the handoff the write just made. It
// runs in the write's transaction and writes nothing for a member or no handler.
func writeHandoffIntent(ctx context.Context, q *db.Queries, rec *statusRecord, issue db.Issue) error {
	seed := rec.intent
	if seed == nil || !issue.AssigneeID.Valid || (issue.AssigneeType.String != "agent" && issue.AssigneeType.String != "squad") {
		return nil
	}
	row, err := q.CreateHandoffDispatch(ctx, db.CreateHandoffDispatchParams{
		ID:                dbid.NewV7(),
		WorkspaceID:       issue.WorkspaceID,
		IssueID:           issue.ID,
		StatusHistoryID:   activityID(rec.activity),
		TargetType:        issue.AssigneeType.String,
		TargetID:          issue.AssigneeID,
		ActorType:         seed.actorType,
		ActorID:           optionalActorUUID(seed.actorID),
		SourceTaskID:      seed.sourceTask,
		ContextCommentIds: seed.commentIDs,
		HandoffWrite:      seed.write,
	})
	if err != nil {
		return fmt.Errorf("record handoff intent: %w", err)
	}
	rec.intent, rec.intentID = nil, row.ID // spent: a reused record must not write a second intent
	return nil
}

func activityID(a *db.ActivityLog) pgtype.UUID {
	if a == nil {
		return pgtype.UUID{}
	}
	return a.ID
}

func recIntentID(rec *statusRecord) pgtype.UUID {
	if rec == nil {
		return pgtype.UUID{}
	}
	return rec.intentID
}

func withHandoffIntent(ctx context.Context, id pgtype.UUID) context.Context {
	if !id.Valid {
		return ctx
	}
	return context.WithValue(ctx, handoffIntentKey{}, id)
}

func handoffIntentFrom(ctx context.Context) pgtype.UUID {
	id, _ := ctx.Value(handoffIntentKey{}).(pgtype.UUID)
	return id
}

// skipHandoffIntent settles a pending intent whose run was not started, with
// the reason. A settled intent is left as it is.
func (h *Handler) skipHandoffIntent(ctx context.Context, intentID pgtype.UUID, reason string) {
	if !intentID.Valid {
		return
	}
	_, err := h.Queries.SettleHandoffDispatch(ctx, db.SettleHandoffDispatchParams{
		ID: intentID, State: handoffSkipped, SkipReason: pgtype.Text{String: reason, Valid: true},
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.Warn("settle handoff intent as skipped failed", "intent_id", util.UUIDToString(intentID), "reason", reason, "error", err)
	}
}

// dispatchHandoffIntent turns a pending handoff intent into exactly one run and
// returns the intent as it stands afterwards. The intent row and the issue row
// are locked for the whole decision, so two dispatchers take turns, and the
// later one reads the first one's run.
func (h *Handler) dispatchHandoffIntent(ctx context.Context, intentID pgtype.UUID) (db.IssueHandoffDispatch, error) {
	if h.TxStarter == nil {
		return db.IssueHandoffDispatch{}, errors.New("transaction starter is not configured")
	}
	intent, err := h.Queries.GetHandoffDispatch(ctx, intentID)
	if err != nil {
		return db.IssueHandoffDispatch{}, fmt.Errorf("read handoff intent: %w", err)
	}
	// The connected-app overlay is built before the transaction opens: it reads
	// the network and must not hold the row locks.
	overlay := h.prepareIntentOverlay(ctx, intent)
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return db.IssueHandoffDispatch{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	q := h.Queries.WithTx(tx)
	// Issue first, as every other path that touches an issue's runs does.
	if _, err := q.LockIssueRunBudget(ctx, intent.IssueID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return db.IssueHandoffDispatch{}, err
	}
	if intent, err = q.LockHandoffDispatch(ctx, intentID); err != nil {
		return db.IssueHandoffDispatch{}, fmt.Errorf("lock handoff intent: %w", err)
	}
	if intent.State != handoffPending {
		return intent, nil
	}
	settled, afterCommit, err := h.settleHandoffIntentInTx(ctx, q, intent, overlay)
	if err != nil {
		return db.IssueHandoffDispatch{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return db.IssueHandoffDispatch{}, err
	}
	if afterCommit != nil {
		afterCommit()
	}
	return settled, nil
}

// settleHandoffIntentInTx finds or starts the intent's run and records the
// outcome on the intent, all with q. The run and its link commit together.
func (h *Handler) settleHandoffIntentInTx(ctx context.Context, q *db.Queries, intent db.IssueHandoffDispatch, overlay *service.PreparedRuntimeMCPOverlay) (db.IssueHandoffDispatch, func(), error) {
	// The receipts the handoff's write left for the run's agent settle in the
	// transaction that records the delivery, so a crash cannot leave a delivered
	// intent with a receipt that triggers a second run.
	dispatched := func(taskID pgtype.UUID) (db.IssueHandoffDispatch, error) {
		if agent := h.intentRunAgent(ctx, q, intent); agent.Valid {
			if err := q.ConsumeHandoffWakeupReceipts(ctx, db.ConsumeHandoffWakeupReceiptsParams{
				IssueID: intent.IssueID, AgentID: agent, Write: intent.HandoffWrite,
			}); err != nil {
				return db.IssueHandoffDispatch{}, fmt.Errorf("settle handoff receipts: %w", err)
			}
		}
		return q.SettleHandoffDispatch(ctx, db.SettleHandoffDispatchParams{ID: intent.ID, State: handoffDispatched, TaskID: taskID})
	}
	skipped := func(reason string) (db.IssueHandoffDispatch, func(), error) {
		row, err := q.SettleHandoffDispatch(ctx, db.SettleHandoffDispatchParams{
			ID: intent.ID, State: handoffSkipped, SkipReason: pgtype.Text{String: reason, Valid: true},
		})
		return row, nil, err
	}
	// The run exists, in whatever state it has reached: that is the delivery.
	if taskID, err := q.GetTaskByHandoffDispatch(ctx, intent.ID); err == nil {
		row, err := dispatched(taskID)
		return row, nil, err
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return db.IssueHandoffDispatch{}, nil, err
	}
	issue, err := q.GetIssue(ctx, intent.IssueID)
	if err != nil {
		return db.IssueHandoffDispatch{}, nil, fmt.Errorf("read issue: %w", err)
	}
	if issue.AssigneeType.String != intent.TargetType || issue.AssigneeID != intent.TargetID {
		return skipped("assignee_changed")
	}
	// The run is built as the original write would have built it: the acting
	// task and the handoff comment ride on the context.
	ctx = intentRunContext(ctx, intent)
	th := h.onQueries(q)
	actorUser := memberActorUserID(intent.ActorType, util.UUIDToString(intent.ActorID))
	var (
		taskID pgtype.UUID
		after  func()
	)
	if intent.TargetType == "squad" {
		taskID, after, err = th.enqueueSquadLeaderRunWithOverlayInTx(ctx, q, issue, pgtype.UUID{}, intent.ActorType, util.UUIDToString(intent.ActorID), overlay)
	} else {
		taskID, after, err = th.TaskService.EnqueueIssueRunInTx(ctx, q, issue, pgtype.UUID{}, actorUser, overlay)
	}
	switch {
	case err == nil:
		if err := q.LinkTaskToHandoffDispatch(ctx, db.LinkTaskToHandoffDispatchParams{DispatchID: intent.ID, TaskID: taskID}); err != nil {
			return db.IssueHandoffDispatch{}, nil, fmt.Errorf("link run to intent: %w", err)
		}
		row, err := dispatched(taskID)
		return row, after, err
	case errors.Is(err, service.ErrDuplicatePendingTask):
		// A sibling run holds the slot. It is the delivery only when it is the
		// queued run for the step the issue is in now.
		if !h.queuedRunCarriesWrite(ctx, issue, h.intentRunAgent(ctx, q, intent)) {
			return skipped("duplicate_pending_run")
		}
		if err := q.LinkTaskToHandoffDispatch(ctx, db.LinkTaskToHandoffDispatchParams{DispatchID: intent.ID, TaskID: taskID}); err != nil {
			return db.IssueHandoffDispatch{}, nil, fmt.Errorf("link sibling run to intent: %w", err)
		}
		row, err := dispatched(taskID)
		return row, nil, err
	case handoffRunRefused(err):
		return skipped(err.Error())
	}
	return db.IssueHandoffDispatch{}, nil, err
}

// handoffRunRefused is an admission refusal: the run was not inserted and will not
// be by another try.
func handoffRunRefused(err error) bool {
	for _, refusal := range []error{
		service.ErrRunBudgetExhausted, service.ErrStageQueueFull, service.ErrAdmissionDeferred, service.ErrReviewHeadAnswered,
		service.ErrIssueInTriage, service.ErrAgentPaused, service.ErrRecoveryCaused, service.ErrAgentArchived, errSquadLeaderNotAdmitted,
	} {
		if errors.Is(err, refusal) {
			return true
		}
	}
	return false
}

// intentRunAgent is the agent whose run the intent asks for: the agent target,
// or the leader of a squad target.
func (h *Handler) intentRunAgent(ctx context.Context, q *db.Queries, intent db.IssueHandoffDispatch) pgtype.UUID {
	if intent.TargetType != "squad" {
		return intent.TargetID
	}
	squad, err := q.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: intent.TargetID, WorkspaceID: intent.WorkspaceID})
	if err != nil {
		return pgtype.UUID{}
	}
	return squad.LeaderID
}

// prepareIntentOverlay builds, outside any transaction, the connected-app overlay
// of the run the intent asks for, for an agent target or a squad leader. The
// run is built as the original write would build it: the acting task and the
// handoff comment ride on the context. If the assignee changed since, the
// insert finds the overlay built for another agent and takes none.
func (h *Handler) prepareIntentOverlay(ctx context.Context, intent db.IssueHandoffDispatch) *service.PreparedRuntimeMCPOverlay {
	issue, err := h.Queries.GetIssue(ctx, intent.IssueID)
	if err != nil {
		return nil
	}
	agent := h.intentRunAgent(ctx, h.Queries, intent)
	ctx = intentRunContext(ctx, intent)
	return h.TaskService.PrepareStatusHandoffRunOverlay(ctx, issue, agent, memberActorUserID(intent.ActorType, util.UUIDToString(intent.ActorID)))
}

// intentRunContext carries the acting task and the handoff comment of the
// write the intent records, as the write's own run would have had them.
func intentRunContext(ctx context.Context, intent db.IssueHandoffDispatch) context.Context {
	if intent.SourceTaskID.Valid {
		ctx = service.WithActingTask(ctx, intent.SourceTaskID)
	}
	if len(intent.ContextCommentIds) > 0 {
		ctx = service.WithStatusHandoffComment(ctx, intent.ContextCommentIds[0])
	}
	return ctx
}
