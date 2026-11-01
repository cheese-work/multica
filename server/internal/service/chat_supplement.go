package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Stable 409 codes for steering a queued DM follow-up into the running task.
const (
	ChatSupplementNoActiveRun          = "no_active_run"
	ChatSupplementUnsupported          = "task_supplement_unsupported"
	ChatSupplementStarting             = "task_supplement_starting"
	ChatSupplementTurnEnded            = "task_supplement_turn_ended"
	ChatSupplementTaskNoLongerQueued   = "task_no_longer_queued"
	ChatSupplementDeliveryUncertain    = "supplement_delivery_uncertain"
	ChatSupplementNotResendable        = "supplement_not_resendable"
	ChatSupplementResentUncertainNotes = "This message may already have reached the previous run. If you already acted on it, do not repeat the action."
)

// ErrChatSupplementNotFound means the message was never steered in this session.
var ErrChatSupplementNotFound = errors.New("chat supplement not found")

// ChatSupplementConflict is a refused steer or resend. Nothing was changed.
type ChatSupplementConflict struct{ Code string }

func (e *ChatSupplementConflict) Error() string { return "chat supplement: " + e.Code }

func chatSupplementConflict(code string) error { return &ChatSupplementConflict{Code: code} }

// ChatSupplementConflictCode returns the stable code of a refusal, if any.
func ChatSupplementConflictCode(err error) (string, bool) {
	var conflict *ChatSupplementConflict
	if errors.As(err, &conflict) {
		return conflict.Code, true
	}
	return "", false
}

// ChatSteerResult is a committed (or replayed) steer.
type ChatSteerResult struct {
	Receipt     db.ChatTaskSupplement
	RunningTask db.AgentTaskQueue
	// Cancelled is the queued follow-up this steer cancelled; nil on a replay.
	Cancelled *db.AgentTaskQueue
}

// lockChatSessionAndAgent takes the send path's lock order (session, then
// agent) and refuses an archived session or an agent that cannot run.
func lockChatSessionAndAgent(ctx context.Context, qtx *db.Queries, session db.ChatSession) (db.Agent, error) {
	if _, err := qtx.LockChatSessionForRuntimeBind(ctx, session.ID); err != nil {
		return db.Agent{}, fmt.Errorf("lock chat session: %w", err)
	}
	current, err := qtx.GetChatSession(ctx, session.ID)
	if err != nil {
		return db.Agent{}, fmt.Errorf("reload chat session: %w", err)
	}
	if current.Status != "active" {
		return db.Agent{}, ErrChatSessionArchived
	}
	agent, err := qtx.GetAgentForClaimUpdate(ctx, session.AgentID)
	if err != nil {
		return db.Agent{}, fmt.Errorf("reload chat agent: %w", err)
	}
	if agent.ArchivedAt.Valid {
		return db.Agent{}, ErrChatTaskAgentArchived
	}
	if !agent.RuntimeID.Valid {
		return db.Agent{}, ErrChatTaskAgentNoRuntime
	}
	return agent, nil
}

// SteerChatTask moves the single message of a queued follow-up into the
// running chat task of the same session.
func (s *TaskService) SteerChatTask(ctx context.Context, session db.ChatSession, queuedTaskID, authorID pgtype.UUID) (ChatSteerResult, error) {
	var out ChatSteerResult
	err := s.runInTx(ctx, func(qtx *db.Queries) error {
		if _, err := lockChatSessionAndAgent(ctx, qtx, session); err != nil {
			return err
		}
		queued, err := qtx.GetAgentTask(ctx, queuedTaskID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && queued.ChatSessionID != session.ID) {
			return chatSupplementConflict(ChatSupplementTaskNoLongerQueued)
		}
		if err != nil {
			return fmt.Errorf("load queued chat task: %w", err)
		}
		// Only a direct send owns exactly one message; a retry child or a
		// channel batch has nothing a single receipt can describe.
		input, err := qtx.ListChatInputMessages(ctx, queued.ID)
		if err != nil {
			return fmt.Errorf("load queued chat input: %w", err)
		}
		if len(input) != 1 || queued.ChatInputTaskID != queued.ID {
			return chatSupplementConflict(ChatSupplementTaskNoLongerQueued)
		}
		message := input[0]
		// Only a resend moves the message to a new task, and it marks the
		// receipt it consumed. A latest receipt that is not resent therefore
		// came from this same queued task: a repeat steer replays it. A
		// resent one is history, and steering the resent task is a new request.
		existing, err := qtx.GetLatestChatTaskSupplement(ctx, db.GetLatestChatTaskSupplementParams{
			ChatSessionID: session.ID, MessageID: message.ID,
		})
		if err == nil && !existing.ResentTaskID.Valid {
			out.Receipt = existing
			out.RunningTask, err = qtx.GetAgentTask(ctx, existing.TaskID)
			return err
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("load chat supplement: %w", err)
		}
		if queued.Status != "queued" {
			return chatSupplementConflict(ChatSupplementTaskNoLongerQueued)
		}

		head, err := steerHead(ctx, qtx, session.ID, queued)
		if err != nil {
			return err
		}
		if _, err := qtx.LockTaskForSupplementBind(ctx, head.ID); err != nil {
			return fmt.Errorf("lock running chat task: %w", err)
		}
		running, err := qtx.GetAgentTask(ctx, head.ID)
		if err != nil {
			return fmt.Errorf("reload running chat task: %w", err)
		}
		if running.Status != "running" {
			return chatSupplementConflict(ChatSupplementTurnEnded)
		}
		capability, err := qtx.GetTaskSupplementCapability(ctx, running.ID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && capability.Capability != protocol.DaemonCapabilityTaskSupplementChatV1) {
			return chatSupplementConflict(ChatSupplementUnsupported)
		}
		if err != nil {
			return fmt.Errorf("load chat supplement capability: %w", err)
		}

		out.Receipt, err = qtx.CreateChatTaskSupplement(ctx, db.CreateChatTaskSupplementParams{
			MessageID: message.ID, TaskID: running.ID, WorkspaceID: session.WorkspaceID,
			ChatSessionID: session.ID, AuthorID: authorID,
		})
		// A confirmed resend of an uncertain receipt can queue behind the run
		// that may already hold the message. One receipt per run and message.
		if isUniqueViolation(err) {
			return chatSupplementConflict(ChatSupplementNotResendable)
		}
		if err != nil {
			return fmt.Errorf("create chat supplement: %w", err)
		}
		// The message keeps task_id = the cancelled follow-up, so it stays in
		// the transcript and no later turn reads it as input again.
		cancelled, err := qtx.CancelQueuedAgentTask(ctx, db.CancelQueuedAgentTaskParams{
			ID:              queued.ID,
			ChatSessionID:   session.ID,
			CancelledByType: pgtype.Text{String: "member", Valid: true},
			CancelledByID:   authorID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return chatSupplementConflict(ChatSupplementTaskNoLongerQueued)
		}
		if err != nil {
			return fmt.Errorf("cancel steered chat task: %w", err)
		}
		if err := SettleTerminalTaskState(ctx, qtx, cancelled); err != nil {
			return err
		}
		out.RunningTask = running
		out.Cancelled = &cancelled
		return nil
	})
	if err != nil {
		return ChatSteerResult{}, err
	}
	if out.Cancelled != nil {
		task := *out.Cancelled
		s.captureTaskCancelled(ctx, task)
		s.ReconcileAgentStatus(ctx, task.AgentID)
		s.broadcastTaskEvent(ctx, protocol.EventTaskCancelled, task)
		s.NotifyTaskFinished(task)
	}
	return out, nil
}

// steerHead returns the claimed task a steer would join, or the refusal that
// explains why there is none.
func steerHead(ctx context.Context, qtx *db.Queries, sessionID pgtype.UUID, queued db.AgentTaskQueue) (db.ListPendingChatTasksForSessionRow, error) {
	pending, err := qtx.ListPendingChatTasksForSession(ctx, sessionID)
	if err != nil {
		return db.ListPendingChatTasksForSessionRow{}, fmt.Errorf("list pending chat tasks: %w", err)
	}
	if len(pending) > 0 && pending[0].ID != queued.ID {
		switch pending[0].Status {
		case "running":
			return pending[0], nil
		case "dispatched", "waiting_local_directory":
			return db.ListPendingChatTasksForSessionRow{}, chatSupplementConflict(ChatSupplementStarting)
		}
	}
	ended, err := qtx.ChatTaskEndedSince(ctx, db.ChatTaskEndedSinceParams{
		ChatSessionID: sessionID, ExcludeTaskID: queued.ID, Since: queued.CreatedAt,
	})
	if err != nil {
		return db.ListPendingChatTasksForSessionRow{}, fmt.Errorf("check ended chat turn: %w", err)
	}
	if ended {
		return db.ListPendingChatTasksForSessionRow{}, chatSupplementConflict(ChatSupplementTurnEnded)
	}
	return db.ListPendingChatTasksForSessionRow{}, chatSupplementConflict(ChatSupplementNoActiveRun)
}

// ChatResendResult is a committed (or replayed) resend.
type ChatResendResult struct {
	Receipt db.ChatTaskSupplement
	Task    db.AgentTaskQueue
	Created bool
}

// ResendChatSupplement queues a steered message as a new turn.
func (s *TaskService) ResendChatSupplement(ctx context.Context, session db.ChatSession, agent db.Agent, messageID, initiatorUserID pgtype.UUID, confirmPossibleDuplicate bool) (ChatResendResult, error) {
	plan, err := s.PlanDirectChatSend(ctx, session.ID, agent, initiatorUserID)
	if err != nil {
		return ChatResendResult{}, err
	}
	var out ChatResendResult
	err = s.runInTx(ctx, func(qtx *db.Queries) error {
		carrier, err := lockChatSessionAndAgent(ctx, qtx, session)
		if err != nil {
			return err
		}
		key := db.GetLatestChatTaskSupplementParams{ChatSessionID: session.ID, MessageID: messageID}
		latest, err := qtx.GetLatestChatTaskSupplement(ctx, key)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrChatSupplementNotFound
		}
		if err != nil {
			return fmt.Errorf("load chat supplement: %w", err)
		}
		// Task row, then receipt: the order claim, ack and settlement use.
		if _, err := qtx.LockTaskForSupplementBind(ctx, latest.TaskID); err != nil {
			return fmt.Errorf("lock chat supplement task: %w", err)
		}
		receipt, err := qtx.LockLatestChatTaskSupplement(ctx, db.LockLatestChatTaskSupplementParams(key))
		if err != nil {
			return fmt.Errorf("lock chat supplement: %w", err)
		}
		out.Receipt = receipt
		if receipt.ResentTaskID.Valid {
			out.Task, err = qtx.GetAgentTask(ctx, receipt.ResentTaskID)
			return err
		}
		switch {
		case receipt.Status == "failed":
		case receipt.Status == "uncertain" && confirmPossibleDuplicate:
		case receipt.Status == "uncertain":
			return chatSupplementConflict(ChatSupplementDeliveryUncertain)
		default:
			return chatSupplementConflict(ChatSupplementNotResendable)
		}
		compacting, err := qtx.HasActiveCompactionTaskForSession(ctx, session.ID)
		if err != nil {
			return fmt.Errorf("check active compaction: %w", err)
		}
		if compacting {
			return ErrChatCompacting
		}

		attr := plan.Attribution
		task, err := qtx.CreateChatTask(ctx, db.CreateChatTaskParams{
			ID:                   dbid.NewV7(),
			AgentID:              session.AgentID,
			RuntimeID:            carrier.RuntimeID,
			Priority:             2,
			ChatSessionID:        session.ID,
			InitiatorUserID:      initiatorUserID,
			OriginatorUserID:     attr.UserID,
			AccountableUserID:    attr.AccountableUserID,
			ForceFreshSession:    pgtype.Bool{Bool: false, Valid: true},
			RuntimeMcpOverlay:    plan.overlay.Overlay,
			RuntimeConnectedApps: plan.overlay.ConnectedApps,
			OriginatorSource:     plan.attrSource,
			TriggerEvidenceKind:  plan.attrEvidenceKind,
			TriggerEvidenceRefID: plan.attrEvidenceRef,
		})
		if err != nil {
			return fmt.Errorf("create resent chat task: %w", err)
		}
		if out.Task, err = qtx.SetChatTaskInputOwnerSelf(ctx, task.ID); err != nil {
			return fmt.Errorf("stamp resent chat input owner: %w", err)
		}
		if err := qtx.LinkChatMessageToTask(ctx, db.LinkChatMessageToTaskParams{ID: messageID, TaskID: task.ID}); err != nil {
			return fmt.Errorf("move resent chat message: %w", err)
		}
		out.Receipt, err = qtx.MarkChatTaskSupplementResent(ctx, db.MarkChatTaskSupplementResentParams{
			ResentTaskID: task.ID, ResentUncertain: receipt.Status == "uncertain",
			MessageID: receipt.MessageID, TaskID: receipt.TaskID,
		})
		if err != nil {
			return fmt.Errorf("mark chat supplement resent: %w", err)
		}
		out.Created = true
		return qtx.TouchChatSession(ctx, session.ID)
	})
	if err != nil {
		return ChatResendResult{}, err
	}
	if out.Created {
		s.RecordHumanDelegation(ctx, plan.Attribution)
		s.NotifyDirectChatSent(ctx, out.Task)
	}
	return out, nil
}
