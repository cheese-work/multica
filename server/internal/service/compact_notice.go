package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// compactNoticeNamespace scopes the UUIDv5 ids of mid-turn compaction notices:
// id = uuidv5(namespace, run message id), so every call for one event derives
// the same row id.
var compactNoticeNamespace = uuid.MustParse("8a0d460e-b5d5-4595-96d0-df9113348370")

func compactNoticeID(runMessageID pgtype.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: uuid.NewSHA1(compactNoticeNamespace, runMessageID.Bytes[:]), Valid: true}
}

// compactNoticeText is the DM line for one `context_compacted` run message. The
// web divider reads the leading "Compacted" and "at Xk tokens" from it.
func compactNoticeText(input []byte) string {
	var counts struct {
		Before int64 `json:"tokens_before"`
		After  int64 `json:"tokens_after"`
	}
	if len(input) > 0 {
		_ = json.Unmarshal(input, &counts)
	}
	switch {
	case counts.Before > 0 && counts.After > 0:
		return fmt.Sprintf("Compacted mid-turn at %dk tokens (kept %dk). Earlier details may be lost.",
			compactTokensK(counts.Before), compactTokensK(counts.After))
	case counts.Before > 0:
		return fmt.Sprintf("Compacted mid-turn at %dk tokens. Earlier details may be lost.", compactTokensK(counts.Before))
	}
	return "Compacted mid-turn. Earlier details may be lost."
}

// ReconcileCompactNotices projects every `context_compacted` run message of a
// web DM's chat task to one `compact_notice` in the DM (CHE-9935). It is
// idempotent: ids derive from the run message ids and the insert skips existing
// rows, so repeated or overlapping calls leave one row per event, and only the
// call that inserted a row publishes it. It works whatever the task status: a
// cancel settles a task before the daemon flushes its transcript, and a late
// event still gets its notice. Channel-bound DMs and compaction tasks get none.
func (s *TaskService) ReconcileCompactNotices(ctx context.Context, taskID pgtype.UUID) ([]db.ChatMessage, error) {
	var (
		task    db.AgentTaskQueue
		created []db.ChatMessage
	)
	err := s.runInTx(ctx, func(qtx *db.Queries) error {
		if err := lockChatSessionForTaskWrite(ctx, qtx, taskID); err != nil {
			return err
		}
		var err error
		if task, err = qtx.GetAgentTask(ctx, taskID); err != nil {
			return fmt.Errorf("get task for compact notices: %w", err)
		}
		if !task.ChatSessionID.Valid || task.TaskKind.String == CompactTaskKind {
			return nil
		}
		sessionID, err := chatReplySessionID(ctx, qtx, task.ChatSessionID)
		if err != nil {
			return err
		}
		if _, err := qtx.GetChannelChatSessionBindingBySessionAny(ctx, sessionID); !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		runMessages, err := qtx.ListTaskContextCompactedMessages(ctx, taskID)
		if err != nil {
			return fmt.Errorf("list context_compacted messages: %w", err)
		}
		for _, event := range runMessages {
			notice, err := qtx.CreateCompactNoticeIdempotent(ctx, db.CreateCompactNoticeIdempotentParams{
				ID:            compactNoticeID(event.ID),
				ChatSessionID: sessionID,
				Content:       compactNoticeText(event.Input),
				EventAt:       event.CreatedAt,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				continue // already written by an earlier or concurrent call
			} else if err != nil {
				return fmt.Errorf("write compact notice: %w", err)
			}
			created = append(created, notice)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(created) == 0 {
		return nil, nil
	}
	scope := s.ResolveTaskScope(ctx, task)
	if scope.WorkspaceID == "" {
		return created, nil
	}
	for _, msg := range created {
		s.Bus.Publish(events.Event{
			Type:               protocol.EventChatMessage,
			WorkspaceID:        scope.WorkspaceID,
			ActorType:          "agent",
			ActorID:            util.UUIDToString(task.AgentID),
			ChatSessionID:      util.UUIDToString(msg.ChatSessionID),
			ChatSessionOwnerID: scope.ChatSessionOwnerID,
			Payload: protocol.ChatMessagePayload{
				ChatSessionID: util.UUIDToString(msg.ChatSessionID),
				MessageID:     util.UUIDToString(msg.ID),
				Role:          msg.Role,
				Content:       msg.Content,
				CreatedAt:     msg.CreatedAt.Time.UTC().Format(time.RFC3339Nano),
				MessageKind:   msg.MessageKind,
			},
		})
	}
	return created, nil
}

// reconcileCompactNoticesBestEffort is the terminal and cancel-finalize backstop
// around ReconcileCompactNotices. The turn is already settled when it runs, so a
// failure is logged and never fails the caller.
func (s *TaskService) reconcileCompactNoticesBestEffort(ctx context.Context, taskID pgtype.UUID) {
	if _, err := s.ReconcileCompactNotices(ctx, taskID); err != nil {
		slog.Warn("reconcile compact notices failed", "task_id", util.UUIDToString(taskID), "error", err)
	}
}
