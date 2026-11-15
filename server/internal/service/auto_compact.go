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
	AutoCompactMinPct = 50
	AutoCompactMaxPct = 90

	// CHE-4331: with no percent saved, the threshold is a token count chosen by
	// the model's window (windows up to 200k, or larger). The larger-window
	// value is the idle trigger; Cheese's rule caps it at 400k.
	AutoCompactSmallWindow       = 200_000
	AutoCompactDefaultTokens200k = 190_000
	AutoCompactDefaultTokens1M   = 200_000
	AutoCompactMinTokens         = 100_000
	AutoCompactMaxTokens200k     = 200_000
	AutoCompactMaxTokens1M       = 400_000

	// CompactTaskKind marks a compaction run, manual or automatic.
	CompactTaskKind = "compact"
)

// ErrChatCompacting refuses a send while a compaction task is active.
var ErrChatCompacting = errors.New("chat is compacting")

// AutoCompact is the effective rule for one DM.
type AutoCompact struct {
	Enabled      bool
	ThresholdPct int
	Tokens200k   int64
	Tokens1M     int64
	// Tokens1MSet is true when a layer saved threshold_tokens_1m; runs use the
	// 400k cap instead of the DM's idle default otherwise.
	Tokens1MSet bool
}

// ValidAutoCompactTokens reports whether v is a whole token count within
// [AutoCompactMinTokens, max].
func ValidAutoCompactTokens(v float64, max int64) bool {
	return v == math.Trunc(v) && v >= AutoCompactMinTokens && v <= float64(max)
}

func ValidAutoCompactPct(v float64) bool {
	return v == math.Trunc(v) && v >= AutoCompactMinPct && v <= AutoCompactMaxPct
}

// ResolveAutoCompact applies the defaults, then the workspace value, then the
// agent override; a field present in a later layer wins. Unparsable layers and
// out-of-range stored values are skipped: both are validated on write, so this
// only guards legacy data.
func ResolveAutoCompact(workspaceSettings, agentConfig []byte) AutoCompact {
	rule := AutoCompact{Enabled: true, Tokens200k: AutoCompactDefaultTokens200k, Tokens1M: AutoCompactDefaultTokens1M}
	for _, raw := range [][]byte{workspaceSettings, agentConfig} {
		var layer struct {
			AutoCompact *struct {
				Enabled      *bool    `json:"enabled"`
				ThresholdPct *float64 `json:"threshold_pct"`
				Tokens200k   *float64 `json:"threshold_tokens_200k"`
				Tokens1M     *float64 `json:"threshold_tokens_1m"`
			} `json:"auto_compact"`
		}
		if json.Unmarshal(raw, &layer) != nil || layer.AutoCompact == nil {
			continue
		}
		l := layer.AutoCompact
		if l.Enabled != nil {
			rule.Enabled = *l.Enabled
		}
		// A layer naming a token field switches the rule back to tokens unless
		// the same layer also names a percent.
		tokenSet := false
		if l.Tokens200k != nil && ValidAutoCompactTokens(*l.Tokens200k, AutoCompactMaxTokens200k) {
			rule.Tokens200k, tokenSet = int64(*l.Tokens200k), true
		}
		if l.Tokens1M != nil && ValidAutoCompactTokens(*l.Tokens1M, AutoCompactMaxTokens1M) {
			rule.Tokens1M, tokenSet, rule.Tokens1MSet = int64(*l.Tokens1M), true, true
		}
		if tokenSet {
			rule.ThresholdPct = 0
		}
		if l.ThresholdPct != nil && ValidAutoCompactPct(*l.ThresholdPct) {
			rule.ThresholdPct = int(*l.ThresholdPct)
		}
	}
	return rule
}

// ThresholdTokens is the prompt size that triggers compaction for a model
// window: the saved percent of the window if one is set, else the token count
// of the window's size class, capped at 95% of a window smaller than it.
func (a AutoCompact) ThresholdTokens(window int64) int64 {
	if a.ThresholdPct > 0 {
		return (int64(a.ThresholdPct)*window + 99) / 100
	}
	limit := a.Tokens1M
	if window <= AutoCompactSmallWindow {
		limit = a.Tokens200k
	}
	return min(limit, window*95/100)
}

// Reached reports whether a prompt of tokens reaches the window's threshold.
func (a AutoCompact) Reached(tokens, window int64) bool {
	return a.Enabled && tokens > 0 && window > 0 && tokens >= a.ThresholdTokens(window)
}

// IsCompactCommand reports whether a message is the /compact command: its
// first word is exactly /compact.
func IsCompactCommand(content string) bool {
	firstLine, _, _ := strings.Cut(strings.TrimSpace(content), "\n")
	words := strings.Fields(firstLine)
	return len(words) > 0 && words[0] == "/compact"
}

func compactTokensK(n int64) int64 { return (n + 500) / 1000 }

func autoCompactReason(tokens, window int64, rule AutoCompact) string {
	if rule.ThresholdPct > 0 {
		return fmt.Sprintf("Auto-compacted: context %dk of %dk tokens (%d%%) reached the %d%% threshold.",
			compactTokensK(tokens), compactTokensK(window), tokens*100/window, rule.ThresholdPct)
	}
	return fmt.Sprintf("Auto-compacted: context %dk of %dk tokens (%d%%) reached the %dk-token threshold.",
		compactTokensK(tokens), compactTokensK(window), tokens*100/window, compactTokensK(rule.ThresholdTokens(window)))
}

// autoCompactNotice is the line the member reads in the DM after an automatic
// compaction (CHE-4438). Compaction happens in place, so it never says "new DM".
func autoCompactNotice(tokens, window int64, rule AutoCompact) string {
	return fmt.Sprintf("Compacted in this DM at %dk tokens (limit %dk). Earlier messages are summarized below.",
		compactTokensK(tokens), compactTokensK(rule.ThresholdTokens(window)))
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
		Type:               protocol.EventChatSessionUpdated,
		WorkspaceID:        workspaceID,
		ActorType:          "system",
		ChatSessionID:      util.UUIDToString(session.ID),
		ChatSessionOwnerID: util.UUIDToString(session.CreatorID),
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
		// A run of an earlier epoch measured history a compact already replaced.
		if task.ChatCompactionEpoch.Int64 != session.CompactionEpoch {
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
		last, err := qtx.GetLastAutoCompactTriggerForSession(ctx, session.ID)
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
		reason := autoCompactReason(runContext.Tokens, runContext.Window, rule)
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
			Content:       autoCompactNotice(runContext.Tokens, runContext.Window, rule),
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
	if !task.CompactReason.Valid {
		return nil
	}
	// The compaction succeeded when the task wrote its divider.
	if _, err := q.GetTaskCompactionSummary(ctx, db.GetTaskCompactionSummaryParams{ChatSessionID: session.ID, TaskID: task.ID}); err == nil {
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("check compaction summary: %w", err)
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
