package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/attribution"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// DM auto-compact (CHE-1803): a web DM compacts itself after a completed run
// whose last prompt reached the configured share of the model's context window.

const (
	AutoCompactMinPct     = 50
	AutoCompactMaxPct     = 90
	autoCompactDefaultPct = 80

	// CompactTaskKind marks a compaction run, manual or automatic.
	CompactTaskKind = "compact"
)

// ErrChatCompacting refuses a send while a compaction task is active.
var ErrChatCompacting = errors.New("chat is compacting")

// AutoCompact is the effective rule for one DM.
type AutoCompact struct {
	Enabled      bool
	ThresholdPct int
}

func ValidAutoCompactPct(v float64) bool {
	return v == math.Trunc(v) && v >= AutoCompactMinPct && v <= AutoCompactMaxPct
}

// ResolveAutoCompact applies the defaults, then the workspace value, then the
// agent override; a field present in a later layer wins. Unparsable layers and
// out-of-range stored values are skipped: both are validated on write, so this
// only guards legacy data.
func ResolveAutoCompact(workspaceSettings, agentConfig []byte) AutoCompact {
	rule := AutoCompact{Enabled: true, ThresholdPct: autoCompactDefaultPct}
	for _, raw := range [][]byte{workspaceSettings, agentConfig} {
		var layer struct {
			AutoCompact *struct {
				Enabled      *bool    `json:"enabled"`
				ThresholdPct *float64 `json:"threshold_pct"`
			} `json:"auto_compact"`
		}
		if json.Unmarshal(raw, &layer) != nil || layer.AutoCompact == nil {
			continue
		}
		if layer.AutoCompact.Enabled != nil {
			rule.Enabled = *layer.AutoCompact.Enabled
		}
		if pct := layer.AutoCompact.ThresholdPct; pct != nil && ValidAutoCompactPct(*pct) {
			rule.ThresholdPct = int(*pct)
		}
	}
	return rule
}

// Reached reports whether a prompt of tokens fills the threshold share of
// window. Integer math, so the boundary is exact.
func (a AutoCompact) Reached(tokens, window int64) bool {
	return a.Enabled && tokens > 0 && window > 0 && tokens*100 >= int64(a.ThresholdPct)*window
}

// IsCompactCommand reports whether a message is the /compact command: its
// first word is exactly /compact.
func IsCompactCommand(content string) bool {
	firstLine, _, _ := strings.Cut(strings.TrimSpace(content), "\n")
	words := strings.Fields(firstLine)
	return len(words) > 0 && words[0] == "/compact"
}

func compactTokensK(n int64) int64 { return (n + 500) / 1000 }

func autoCompactReason(tokens, window int64, thresholdPct int) string {
	return fmt.Sprintf("Auto-compacted: context %dk of %dk tokens (%d%%) reached the %d%% threshold.",
		compactTokensK(tokens), compactTokensK(window), tokens*100/window, thresholdPct)
}

// syncChatCompacting tells clients whether the DM of a compaction task is
// compacting, from the task's own status. Called for every task event, so each
// start and each terminal path reports it without per-path wiring.
func (s *TaskService) syncChatCompacting(workspaceID string, task db.AgentTaskQueue) {
	session, err := s.Queries.GetChatSession(context.Background(), task.ChatSessionID)
	if err != nil {
		return
	}
	switch task.Status {
	case "queued", "dispatched", "running", "waiting_local_directory", "deferred":
		s.publishChatCompacting(workspaceID, session, true)
	default:
		s.publishChatCompacting(workspaceID, session, false)
	}
}

func (s *TaskService) publishChatCompacting(workspaceID string, session db.ChatSession, compacting bool) {
	s.Bus.Publish(events.Event{
		Type:          protocol.EventChatSessionUpdated,
		WorkspaceID:   workspaceID,
		ActorType:     "system",
		ChatSessionID: util.UUIDToString(session.ID),
		Payload: protocol.ChatSessionUpdatedPayload{
			ChatSessionID: util.UUIDToString(session.ID),
			Title:         session.Title,
			UpdatedAt:     util.TimestampToString(session.UpdatedAt),
			Compacting:    &compacting,
		},
	})
}

// maybeAutoCompact runs after a task's completion has committed. It queues one
// compaction task and writes the old-DM notice in a single transaction. A
// failure only loses the compaction: the completion already stands.
func (s *TaskService) maybeAutoCompact(ctx context.Context, task db.AgentTaskQueue, runContext *RunContext) {
	if runContext == nil || !task.ChatSessionID.Valid || task.TaskKind.String == CompactTaskKind || task.RegenerateQuickActionsFor.Valid {
		return
	}
	var queued *db.AgentTaskQueue
	err := s.runInTx(ctx, func(qtx *db.Queries) error {
		session, err := qtx.LockChatSessionForEnqueue(ctx, task.ChatSessionID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		if session.Status != "active" || session.ContinuedIntoSessionID.Valid {
			return nil
		}
		// A run that started before the DM's last in-place compaction (for
		// example the one that wrote the summary) is not evidence that the new
		// epoch has grown.
		if task.ChatCompactionEpoch.Int64 < session.CompactionEpoch {
			return nil
		}
		// Web DMs only: a channel-bound chat has an external delivery route.
		if _, err := qtx.GetChannelChatSessionBindingBySessionAny(ctx, session.ID); !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		workspace, err := qtx.GetWorkspace(ctx, session.WorkspaceID)
		if err != nil {
			return err
		}
		agent, err := qtx.GetAgent(ctx, session.AgentID)
		if err != nil {
			return err
		}
		rule := ResolveAutoCompact(workspace.Settings, agent.RuntimeConfig)
		if !rule.Reached(runContext.Tokens, runContext.Window) || agent.ArchivedAt.Valid || !agent.RuntimeID.Valid {
			return nil
		}
		active, err := qtx.HasActiveCompactionTaskForSession(ctx, session.ID)
		if err != nil || active {
			return err
		}
		// After a failed attempt, wait until the context has grown 10% past the
		// size that triggered it, so a failing compaction cannot loop.
		last, err := qtx.GetLastAutoCompactTriggerForSession(ctx, db.GetLastAutoCompactTriggerForSessionParams{
			ChatSessionID:   session.ID,
			CompactionEpoch: session.CompactionEpoch,
		})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if last.Valid && runContext.Tokens*10 < last.Int64*11 {
			return nil
		}

		attr, err := s.applyAttributionFallback(ctx, attribution.DirectHumanRun(session.CreatorID, attribution.EvidenceChat, session.ID), agent)
		if err != nil {
			return err
		}
		attrSource, _, attrEvidenceKind, attrEvidenceRef := attributionCreateParams(attr)
		created, err := qtx.CreateChatTask(ctx, db.CreateChatTaskParams{
			ID:                   dbid.NewV7(),
			AgentID:              session.AgentID,
			RuntimeID:            agent.RuntimeID,
			Priority:             2,
			ChatSessionID:        session.ID,
			InitiatorUserID:      session.CreatorID,
			OriginatorUserID:     attr.UserID,
			AccountableUserID:    attr.AccountableUserID,
			ForceFreshSession:    pgtype.Bool{Bool: false, Valid: true},
			OriginatorSource:     attrSource,
			TriggerEvidenceKind:  attrEvidenceKind,
			TriggerEvidenceRefID: attrEvidenceRef,
		})
		if err != nil {
			return err
		}
		reason := autoCompactReason(runContext.Tokens, runContext.Window, rule.ThresholdPct)
		if created, err = qtx.SetChatTaskInputOwnerSelf(ctx, created.ID); err != nil {
			return err
		}
		if created, err = qtx.MarkChatTaskCompact(ctx, db.MarkChatTaskCompactParams{
			ID:                   created.ID,
			CompactReason:        pgtype.Text{String: reason, Valid: true},
			CompactTriggerTokens: pgtype.Int8{Int64: runContext.Tokens, Valid: true},
		}); err != nil {
			return err
		}
		if _, err := qtx.CreateChatMessage(ctx, db.CreateChatMessageParams{
			ID:            dbid.NewV7(),
			ChatSessionID: session.ID,
			Role:          "assistant",
			Content:       reason + " Continuing in a new DM.",
			MessageKind:   pgtype.Text{String: protocol.ChatMessageKindCompactNotice, Valid: true},
		}); err != nil {
			return err
		}
		queued = &created
		return nil
	})
	if err != nil {
		slog.Warn("auto-compact skipped", "task_id", util.UUIDToString(task.ID), "chat_session_id", util.UUIDToString(task.ChatSessionID), "error", err)
		return
	}
	if queued == nil {
		return
	}
	// Queues the task event, which also tells clients the DM is compacting.
	s.NotifyDirectChatSent(ctx, *queued)
}

// settleCompactionTask is the failure guard: a compaction task that reaches any
// terminal state without having continued or summarised the DM leaves the DM as it was, so an
// automatic one says so and the person can retry with /compact. It runs inside
// SettleTerminalTaskState, the single entry point of every terminal path.
func settleCompactionTask(ctx context.Context, q *db.Queries, task db.AgentTaskQueue) error {
	if task.TaskKind.String != CompactTaskKind || !task.ChatSessionID.Valid {
		return nil
	}
	session, err := q.GetChatSession(ctx, task.ChatSessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return fmt.Errorf("load chat session for compaction guard: %w", err)
	}
	if session.ContinuedIntoSessionID.Valid || !task.CompactReason.Valid {
		return nil
	}
	// In-place compaction ends with a summary owned by this task, not with a
	// continued DM. Explicit "chat continue" still ends the other way.
	if _, err := q.GetTaskCompactionSummary(ctx, db.GetTaskCompactionSummaryParams{
		ChatSessionID: session.ID,
		TaskID:        task.ID,
	}); err == nil {
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("load compaction summary for compaction guard: %w", err)
	}
	if _, err := q.CreateChatMessage(ctx, db.CreateChatMessageParams{
		ID:            dbid.NewV7(),
		ChatSessionID: session.ID,
		Role:          "assistant",
		Content:       fmt.Sprintf("Auto-compact failed (%s). Type /compact to retry.", task.Status),
		MessageKind:   pgtype.Text{String: protocol.ChatMessageKindCompactNotice, Valid: true},
	}); err != nil {
		return fmt.Errorf("write compaction failure notice: %w", err)
	}
	return nil
}
