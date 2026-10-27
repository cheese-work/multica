package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	MaxPendingChatWakeups          = 3
	MaxChatWakeupDelay             = 24 * time.Hour
	MaxChatWakeupInstruction       = 4000
	chatWakeupTickBatch      int32 = 50
	chatWakeupSentVia              = "system"
)

var (
	ErrChatWakeupNotOwner = errors.New("chat wakeup: only a running task of this web DM can manage its wakeups")
	ErrChatWakeupLimit    = errors.New("chat wakeup: too many pending wakeups in this DM")
	ErrChatWakeupTooFar   = errors.New("chat wakeup: fire time is more than 24h away")
	ErrChatWakeupPast     = errors.New("chat wakeup: fire time must be in the future")
	ErrChatWakeupEmpty    = errors.New("chat wakeup: instruction is required")
	ErrChatWakeupTooLong  = errors.New("chat wakeup: instruction is too long")
	ErrChatWakeupNotFound = errors.New("chat wakeup: no such pending wakeup")
)

// ChatWakeupService schedules one-shot turns an agent asks for in its own web
// DM. The task that asks identifies the DM, so an agent can only reach the DM
// it is currently running in.
type ChatWakeupService struct {
	Tasks *TaskService
}

type ChatWakeupInput struct {
	TaskID      pgtype.UUID
	At          time.Time
	Instruction string
}

// session resolves the active web DM that a running chat task belongs to.
func (s *ChatWakeupService) session(ctx context.Context, q *db.Queries, taskID pgtype.UUID) (db.ChatSession, error) {
	task, err := q.GetAgentTask(ctx, taskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.ChatSession{}, ErrChatWakeupNotOwner
	}
	if err != nil {
		return db.ChatSession{}, fmt.Errorf("load task: %w", err)
	}
	if !task.ChatSessionID.Valid || (task.Status != "running" && task.Status != "dispatched") {
		return db.ChatSession{}, ErrChatWakeupNotOwner
	}
	session, err := q.GetChatSession(ctx, task.ChatSessionID)
	if err != nil {
		return db.ChatSession{}, fmt.Errorf("load chat session: %w", err)
	}
	if session.AgentID != task.AgentID || session.Status != "active" {
		return db.ChatSession{}, ErrChatWakeupNotOwner
	}
	if _, err := q.GetChannelChatSessionBindingBySessionAny(ctx, session.ID); !errors.Is(err, pgx.ErrNoRows) {
		if err != nil {
			return db.ChatSession{}, fmt.Errorf("check DM source: %w", err)
		}
		return db.ChatSession{}, ErrChatWakeupNotOwner
	}
	return session, nil
}

func (s *ChatWakeupService) Create(ctx context.Context, in ChatWakeupInput) (db.ChatWakeup, error) {
	instruction := strings.TrimSpace(in.Instruction)
	now := time.Now()
	switch {
	case instruction == "":
		return db.ChatWakeup{}, ErrChatWakeupEmpty
	case len(instruction) > MaxChatWakeupInstruction:
		return db.ChatWakeup{}, ErrChatWakeupTooLong
	case !in.At.After(now):
		return db.ChatWakeup{}, ErrChatWakeupPast
	case in.At.After(now.Add(MaxChatWakeupDelay)):
		return db.ChatWakeup{}, ErrChatWakeupTooFar
	}
	var out db.ChatWakeup
	err := s.Tasks.runInTx(ctx, func(qtx *db.Queries) error {
		session, err := s.session(ctx, qtx, in.TaskID)
		if err != nil {
			return err
		}
		// The session lock serializes creators, so the cap cannot be raced past.
		// An archive that committed while this waited is only visible on the
		// locked row, so status is re-checked here.
		locked, err := qtx.LockChatSessionForEnqueue(ctx, session.ID)
		if err != nil {
			return fmt.Errorf("lock chat session: %w", err)
		}
		if locked.Status != "active" {
			return ErrChatWakeupNotOwner
		}
		pending, err := qtx.CountPendingChatWakeups(ctx, session.ID)
		if err != nil {
			return fmt.Errorf("count pending wakeups: %w", err)
		}
		if pending >= MaxPendingChatWakeups {
			return ErrChatWakeupLimit
		}
		out, err = qtx.CreateChatWakeup(ctx, db.CreateChatWakeupParams{
			WorkspaceID:     session.WorkspaceID,
			ChatSessionID:   session.ID,
			AgentID:         session.AgentID,
			Instruction:     instruction,
			FireAt:          pgtype.Timestamptz{Time: in.At, Valid: true},
			CreatedByTaskID: in.TaskID,
		})
		return err
	})
	return out, err
}

func (s *ChatWakeupService) List(ctx context.Context, taskID pgtype.UUID) ([]db.ChatWakeup, error) {
	session, err := s.session(ctx, s.Tasks.Queries, taskID)
	if err != nil {
		return nil, err
	}
	return s.Tasks.Queries.ListPendingChatWakeups(ctx, session.ID)
}

func (s *ChatWakeupService) Cancel(ctx context.Context, taskID, wakeupID pgtype.UUID) error {
	session, err := s.session(ctx, s.Tasks.Queries, taskID)
	if err != nil {
		return err
	}
	if _, err := s.Tasks.Queries.CancelChatWakeup(ctx, db.CancelChatWakeupParams{ID: wakeupID, ChatSessionID: session.ID}); errors.Is(err, pgx.ErrNoRows) {
		return ErrChatWakeupNotFound
	} else if err != nil {
		return fmt.Errorf("cancel wakeup: %w", err)
	}
	return nil
}

// Tick fires every due wakeup. One wakeup's failure never blocks the others;
// a wakeup that could not fire stays pending and is retried on the next tick.
func (s *ChatWakeupService) Tick(ctx context.Context) error {
	due, err := s.Tasks.Queries.ListDueChatWakeups(ctx, chatWakeupTickBatch)
	if err != nil {
		return fmt.Errorf("list due chat wakeups: %w", err)
	}
	var errs []error
	for _, w := range due {
		if err := s.fire(ctx, w); err != nil {
			slog.Warn("chat wakeup fire failed", "wakeup_id", util.UUIDToString(w.ID), "error", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func chatWakeupContent(instruction string) string {
	return "[Scheduled wakeup — you set this earlier in this DM; no person wrote this message.]\nInstruction: " + instruction
}

func (s *ChatWakeupService) fire(ctx context.Context, w db.ChatWakeup) error {
	q := s.Tasks.Queries
	drop := func() error { return q.DropChatWakeup(ctx, w.ID) }

	session, err := q.GetChatSession(ctx, w.ChatSessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return drop()
	}
	if err != nil {
		return fmt.Errorf("load chat session: %w", err)
	}
	if session.Status != "active" {
		return drop()
	}
	agent, err := q.GetAgent(ctx, session.AgentID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (agent.ArchivedAt.Valid || !agent.RuntimeID.Valid)) {
		return drop()
	}
	if err != nil {
		return fmt.Errorf("load agent: %w", err)
	}
	plan, err := s.Tasks.PlanDirectChatSend(ctx, session.ID, agent, session.CreatorID)
	if err != nil {
		return fmt.Errorf("plan scheduled turn: %w", err)
	}

	var sent DirectChatSendResult
	claimed := false
	err = s.Tasks.runInTx(ctx, func(qtx *db.Queries) error {
		if _, err := qtx.ClaimChatWakeup(ctx, w.ID); errors.Is(err, pgx.ErrNoRows) {
			return nil // another tick or a cancel got there first
		} else if err != nil {
			return err
		}
		var err error
		sent, err = s.Tasks.SendDirectChatMessageTx(ctx, qtx, plan, session, session.CreatorID,
			chatWakeupContent(w.Instruction), nil, "", pgtype.UUID{}, chatWakeupSentVia)
		if err != nil {
			return err
		}
		claimed = true
		return qtx.SetChatWakeupFiredTask(ctx, db.SetChatWakeupFiredTaskParams{ID: w.ID, FiredTaskID: sent.Task.ID})
	})
	switch {
	case errors.Is(err, ErrChatSessionArchived), errors.Is(err, ErrChatTaskAgentArchived), errors.Is(err, ErrChatTaskAgentNoRuntime):
		return drop()
	case err != nil:
		return err // includes ErrChatCompacting: stays pending, retried next tick
	case !claimed:
		return nil
	}
	s.Tasks.NotifyDirectChatSent(ctx, sent.Task)
	if s.Tasks.Bus != nil {
		sessionID := util.UUIDToString(session.ID)
		s.Tasks.Bus.Publish(events.Event{
			Type:          protocol.EventChatMessage,
			WorkspaceID:   util.UUIDToString(session.WorkspaceID),
			ActorType:     "system",
			ChatSessionID: sessionID,
			Payload: protocol.ChatMessagePayload{
				ChatSessionID: sessionID,
				MessageID:     util.UUIDToString(sent.Message.ID),
				Role:          "user",
				Content:       sent.Message.Content,
				TaskID:        util.UUIDToString(sent.Task.ID),
				CreatedAt:     util.TimestampToString(sent.Message.CreatedAt),
			},
		})
	}
	return nil
}
