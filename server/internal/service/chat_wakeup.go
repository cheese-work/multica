package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// DM wakeup (CHE-1874): an agent in a web DM schedules its own follow-up turn.
// The scheduler starts it as an ordinary DM turn whose input is a hidden
// wakeup_trigger message, so it runs with the DM's history and raises the unread
// badge through the normal completion path.

const (
	ChatWakeupMaxPending     = 3
	ChatWakeupMaxAhead       = 24 * time.Hour
	ChatWakeupMaxInstruction = 12000

	chatWakeupTickBatch = 50
	chatWakeupMaxHops   = 5
)

var (
	ErrChatWakeupInput = errors.New("invalid chat wakeup")
	ErrChatWakeupCap   = errors.New("this DM already has the maximum number of pending wakeups")
	// ErrChatWakeupUnavailable: the DM cannot take a wakeup (archived, continued
	// or bound to a channel).
	ErrChatWakeupUnavailable = errors.New("wakeups are only available in an active web DM")

	errChatWakeupRetry = errors.New("chat wakeup: retry next tick")
)

type ChatWakeupService struct{ Tasks *TaskService }

// Create validates and stores a wakeup for the DM, under the session lock so
// the pending cap cannot be raced past.
func (s *ChatWakeupService) Create(ctx context.Context, sessionID, sourceTaskID pgtype.UUID, instruction string, fireAt, now time.Time) (db.ChatWakeup, error) {
	bad := func(msg string) (db.ChatWakeup, error) {
		return db.ChatWakeup{}, fmt.Errorf("%w: %s", ErrChatWakeupInput, msg)
	}
	instruction = strings.TrimSpace(instruction)
	switch {
	case instruction == "" || len(instruction) > ChatWakeupMaxInstruction || !utf8.ValidString(instruction):
		return bad(fmt.Sprintf("instruction must be UTF-8 text of 1–%d bytes", ChatWakeupMaxInstruction))
	case !fireAt.After(now):
		return bad("the wakeup time must be in the future")
	case fireAt.Sub(now) > ChatWakeupMaxAhead:
		return bad("the wakeup time must be within 24 hours")
	}
	var out db.ChatWakeup
	err := s.Tasks.runInTx(ctx, func(qtx *db.Queries) error {
		session, err := qtx.LockChatSessionForEnqueue(ctx, sessionID)
		if err != nil {
			return err
		}
		if session.Status != "active" || session.ContinuedIntoSessionID.Valid || s.channelBound(ctx, qtx, session.ID) {
			return ErrChatWakeupUnavailable
		}
		pending, err := qtx.CountPendingChatWakeups(ctx, session.ID)
		if err != nil {
			return err
		}
		if pending >= ChatWakeupMaxPending {
			return ErrChatWakeupCap
		}
		out, err = qtx.CreateChatWakeup(ctx, db.CreateChatWakeupParams{
			WorkspaceID:   session.WorkspaceID,
			ChatSessionID: session.ID,
			AgentID:       session.AgentID,
			CreatedBy:     session.CreatorID,
			SourceTaskID:  sourceTaskID,
			Instruction:   instruction,
			FireAt:        pgtype.Timestamptz{Time: fireAt.UTC(), Valid: true},
		})
		return err
	})
	return out, err
}

// channelBound is true when an external channel delivers this DM's replies.
// A failed lookup counts as bound: the wakeup is refused rather than risked.
func (s *ChatWakeupService) channelBound(ctx context.Context, q *db.Queries, sessionID pgtype.UUID) bool {
	_, err := q.GetChannelChatSessionBindingBySessionAny(ctx, sessionID)
	return !errors.Is(err, pgx.ErrNoRows)
}

// Tick fires every due wakeup. A failure on one is logged and retried next
// tick; it never blocks the others.
func (s *ChatWakeupService) Tick(ctx context.Context) error {
	due, err := s.Tasks.Queries.ListDueChatWakeups(ctx, chatWakeupTickBatch)
	if err != nil {
		return fmt.Errorf("list due chat wakeups: %w", err)
	}
	for _, w := range due {
		if err := s.fire(ctx, w); err != nil && !errors.Is(err, errChatWakeupRetry) {
			slog.Warn("chat wakeup not fired", "wakeup_id", util.UUIDToString(w.ID), "error", err)
		}
	}
	return nil
}

// fire starts one wakeup's turn in the DM's current session. Preflight reads
// (continuation chain, agent, attribution) run before the transaction; the
// transaction re-checks them under the session lock.
func (s *ChatWakeupService) fire(ctx context.Context, w db.ChatWakeup) error {
	q := s.Tasks.Queries
	expire := func() error {
		return q.SettleChatWakeup(ctx, db.SettleChatWakeupParams{ID: w.ID, Status: "expired"})
	}
	sessionID := w.ChatSessionID
	var session db.ChatSession
	for hop := 0; ; hop++ {
		var err error
		if session, err = q.GetChatSession(ctx, sessionID); errors.Is(err, pgx.ErrNoRows) {
			return expire()
		} else if err != nil {
			return err
		}
		if !session.ContinuedIntoSessionID.Valid {
			break
		}
		if hop == chatWakeupMaxHops {
			return fmt.Errorf("continuation chain longer than %d", chatWakeupMaxHops)
		}
		sessionID = session.ContinuedIntoSessionID
	}
	if session.Status != "active" || s.channelBound(ctx, q, session.ID) {
		return expire()
	}
	agent, err := q.GetAgent(ctx, session.AgentID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (agent.ArchivedAt.Valid || !agent.RuntimeID.Valid)) {
		return expire()
	} else if err != nil {
		return err
	}
	plan, err := s.Tasks.PlanDirectChatSend(ctx, session.ID, agent, session.CreatorID)
	if err != nil {
		return err
	}

	var started *db.AgentTaskQueue
	err = s.Tasks.runInTx(ctx, func(qtx *db.Queries) error {
		locked, err := qtx.LockPendingChatWakeup(ctx, w.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // cancelled or taken by another tick
		} else if err != nil {
			return err
		}
		current, err := qtx.LockChatSessionForEnqueue(ctx, session.ID)
		if err != nil {
			return err
		}
		if current.Status != "active" || current.ContinuedIntoSessionID.Valid {
			return errChatWakeupRetry // the DM moved since the preflight
		}
		if locked.ChatSessionID != current.ID {
			if err := qtx.MoveChatWakeupToSession(ctx, db.MoveChatWakeupToSessionParams{ID: locked.ID, ChatSessionID: current.ID}); err != nil {
				return err
			}
		}
		sent, err := s.Tasks.sendDirectChatMessageTx(ctx, qtx, plan, current, current.CreatorID, chatWakeupPrompt(locked), nil, "", pgtype.UUID{}, "system", true)
		if err != nil {
			return err
		}
		started = &sent.Task
		return qtx.SettleChatWakeup(ctx, db.SettleChatWakeupParams{ID: locked.ID, Status: "fired", TaskID: sent.Task.ID})
	})
	switch {
	case errors.Is(err, ErrChatCompacting):
		return errChatWakeupRetry // the compaction continues the DM; the next tick follows it
	case errors.Is(err, ErrChatSessionArchived), errors.Is(err, ErrChatTaskAgentArchived), errors.Is(err, ErrChatTaskAgentNoRuntime):
		return expire()
	case err != nil:
		return err
	}
	if started != nil {
		s.Tasks.NotifyDirectChatSent(ctx, *started)
	}
	return nil
}

// chatWakeupPrompt is the hidden input of a wakeup turn: the agent's own
// instruction, framed so it is not mistaken for something the person typed.
func chatWakeupPrompt(w db.ChatWakeup) string {
	return fmt.Sprintf("Scheduled wakeup, not a message from the person. You set it earlier in this DM for %s. "+
		"Carry out your instruction, then reply to the person here.\n\nYour instruction:\n\n%s",
		w.FireAt.Time.UTC().Format(time.RFC3339), w.Instruction)
}
