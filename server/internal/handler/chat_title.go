package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/bgagents"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// chatTitleGenTimeout bounds queueing the dm_title job (workspace read, chain
// reachability, task insert). Kept short: a chat title is a nicety.
const chatTitleGenTimeout = 20 * time.Second

// chatTitleJobInputMaxRunes caps the user message folded into the dm_title job input.
const chatTitleJobInputMaxRunes = 2000

// ChannelChatStarted publishes list invalidation metadata without changing any
// client's current navigation. The explicit empty Chat is already committed.
func (h *Handler) ChannelChatStarted(event engine.ChannelChatStartedEvent) {
	ws, user, session := uuidToString(event.WorkspaceID), uuidToString(event.CreatorID), uuidToString(event.SessionID)
	h.publishChat(protocol.EventChatSessionCreated, ws, "member", user, session, protocol.ChatSessionCreatedPayload{
		WorkspaceID: ws, ChatSessionID: session, AgentID: uuidToString(event.AgentID), CreatorID: user, Title: event.Title,
		ChannelSource: protocol.ChatSessionChannelSource{
			ChannelType: string(event.ChannelType), InstallationID: uuidToString(event.InstallationID), RouteRevision: event.RouteRevision,
		},
		IsCurrentChannelRoute: true,
	})
}

// ChannelChatTitleInitialized publishes the deterministic first-message or
// media fallback immediately. LLM title generation is optional, so clients
// must not depend on its later CAS update to observe the committed title.
func (h *Handler) ChannelChatTitleInitialized(workspaceID, creatorID, sessionID pgtype.UUID, title string) {
	h.publishChat(protocol.EventChatSessionUpdated, uuidToString(workspaceID), "member", uuidToString(creatorID), uuidToString(sessionID), protocol.ChatSessionUpdatedPayload{
		ChatSessionID: uuidToString(sessionID),
		Title:         title,
	})
}

// GenerateChannelChatTitle queues the dm_title job after the
// shared engine has persisted its deterministic first-message fallback.
func (h *Handler) GenerateChannelChatTitle(workspaceID, creatorID, sessionID pgtype.UUID, currentTitle, sourceText string) {
	h.maybeGenerateChatTitleAsync(uuidToString(workspaceID), uuidToString(creatorID), sessionID, currentTitle, sourceText)
}

// maybeGenerateChatTitleAsync runs queueChatTitleJob off the caller's path: the
// workspace read and the enqueue can take up to chatTitleGenTimeout, which must
// not delay the first-message response or the channel inbound turn. A panic is
// logged and dropped; the fallback title stands.
func (h *Handler) maybeGenerateChatTitleAsync(workspaceID, userID string, sessionID pgtype.UUID, currentTitle, sourceText string) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("dm title job panicked", "session_id", uuidToString(sessionID), "panic", r)
			}
		}()
		h.queueChatTitleJob(workspaceID, userID, sessionID, currentTitle, sourceText)
	}()
}

// chatTitleSystemPrompt instructs the model to condense the opening of a
// conversation into a short, language-matched title. The rules mirror the
// acceptance criteria in MUL-4295: no quotes, no trailing punctuation, no
// label prefix, follow the conversation's language. sanitizeChatTitle re-applies
// these rules defensively in case the model ignores them.
//
// This text names no language and contains no CJK, deliberately. A prompt that
// spells out a specific language — even only as a formatting example — reads as
// permission to answer in it, which is how quick actions ended up emitting
// Chinese pills for English conversations (MUL-5689). The label prefixes this
// used to enumerate are still stripped for real by chatTitleLabelPrefixes,
// which is where that guarantee belongs.
const chatTitleSystemPrompt = `You write a very short title that summarizes the topic of a chat conversation, given the user's opening message.

Rules:
- Output ONLY the title text — nothing else, no explanation.
- Keep it short: a few words, ideally under 8, never a full sentence.
- Write the title in the SAME language as the user's message, and in no other.
- Do NOT wrap the title in quotes or brackets.
- Do NOT prefix it with a label such as "Title:", in any language.
- Do NOT end with a period or any trailing punctuation.`

// queueChatTitleJob queues the dm_title background job for a chat session's
// opening message and returns once the task is queued. maybeGenerateChatTitleAsync
// calls it from the first-user-message path of SendChatMessage and the channel hook.
//
//   - Non-blocking for users: the title is written later, when the daemon
//     completes the task. No LLM call runs in this process.
//   - Silent fallback: with no dm_title agent configured or reachable, nothing is
//     queued and the deterministic first-message title stays.
//   - No clobber: the task carries currentTitle as its guard, and writeDMTitle
//     writes by compare-and-swap, so a rename during the run wins.
func (h *Handler) queueChatTitleJob(workspaceID, userID string, sessionID pgtype.UUID, currentTitle, sourceText string) {
	if strings.TrimSpace(sourceText) == "" {
		return
	}
	ws, err := util.ParseUUID(workspaceID)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), chatTitleGenTimeout)
	defer cancel()

	chain, source := h.dmTitleChain(ctx, ws)
	if len(chain) == 0 {
		return
	}
	member, _ := util.ParseUUID(userID)
	// The cap bounds the whole queued input; the message gets what the prompt leaves.
	prefix := chatTitleSystemPrompt + "\n\nThe user's opening message:\n"
	message := []rune(sourceText)
	if room := chatTitleJobInputMaxRunes - len([]rune(prefix)); len(message) > room {
		message = message[:max(room, 0)]
	}
	input := prefix + string(message)
	_, err = h.TaskService.EnqueueBackgroundChain(ctx, service.BackgroundTaskInput{
		Job: bgagents.DMTitle, WorkspaceID: ws, MemberID: member, SourceChatSessionID: sessionID,
		Source: source, Input: input, Guard: currentTitle,
	}, chain)
	if err != nil {
		// ErrNoReachableBackgroundAgent included: the fallback title stands.
		slog.Warn("dm title job not queued; keeping original title",
			"session_id", uuidToString(sessionID), "error", err)
	}
}

// dmTitleChain resolves the dm_title chain from the workspace setting. There is
// no personal config yet, so the user config is empty. Reachability is checked
// by EnqueueBackgroundChain.
func (h *Handler) dmTitleChain(ctx context.Context, ws pgtype.UUID) ([]bgagents.Candidate, bgagents.Source) {
	row, err := h.Queries.GetWorkspace(ctx, ws)
	if err != nil {
		return nil, bgagents.NoAgent
	}
	var settings map[string]json.RawMessage
	if json.Unmarshal(row.Settings, &settings) != nil || len(settings[backgroundAgentsKey]) == 0 {
		return nil, bgagents.NoAgent
	}
	cfg, err := bgagents.Parse(settings[backgroundAgentsKey], bgagents.WorkspaceScope)
	if err != nil {
		return nil, bgagents.NoAgent
	}
	res := bgagents.Resolve(bgagents.DMTitle, nil, cfg, func(bgagents.Source, string) bool { return true })
	return res.Chain, res.Source
}

// writeDMTitle is the dm_title BackgroundWriter. It runs in the completion
// transaction: sanitize the output, then compare-and-swap it onto the DM against
// the title observed at enqueue (the task guard). An output that sanitizes to
// nothing is rejected; a lost CAS (rename won) writes nothing.
func (h *Handler) writeDMTitle(ctx context.Context, q *db.Queries, task db.AgentTaskQueue, result []byte) error {
	var res TaskCompleteRequest
	if err := json.Unmarshal(result, &res); err != nil {
		return fmt.Errorf("%w: %w", service.ErrInvalidBackgroundOutput, err)
	}
	title := sanitizeChatTitle(res.Output)
	if title == "" || !task.SourceChatSessionID.Valid {
		return service.ErrInvalidBackgroundOutput
	}
	var tc service.BackgroundTaskContext
	if err := json.Unmarshal(task.Context, &tc); err != nil {
		return fmt.Errorf("%w: %w", service.ErrInvalidBackgroundOutput, err)
	}
	updated, err := q.UpdateChatSessionTitleIfCurrent(ctx, db.UpdateChatSessionTitleIfCurrentParams{
		ID: task.SourceChatSessionID, ExpectedTitle: tc.Guard, NewTitle: title,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// ponytail: published before the completion transaction commits; the
	// payload carries the title, so clients patch from it. A rollback here
	// would leave a stale title until the next refetch.
	sessionID := uuidToString(updated.ID)
	h.publishChat(protocol.EventChatSessionUpdated, tc.WorkspaceID, "member", uuidToString(task.OriginatorUserID), sessionID, protocol.ChatSessionUpdatedPayload{
		ChatSessionID: sessionID,
		Title:         updated.Title,
		UpdatedAt:     timestampToString(updated.UpdatedAt),
	})
	return nil
}

// chatTitleLabelPrefixes are leading labels a model sometimes prepends despite
// the prompt. Matched case-insensitively and only at the very start.
var chatTitleLabelPrefixes = []string{
	"title:", "title：",
	"标题:", "标题：",
	"题目:", "题目：",
	"主题:", "主题：",
}

// sanitizeChatTitle defensively enforces the title formatting rules regardless
// of how well the model followed the prompt: collapse whitespace, strip a
// leading "Title:" / "标题：" style label, remove surrounding quote/bracket
// pairs, drop trailing sentence punctuation, and hard-cap the length at
// chatSessionTitleMaxLen runes (the same ceiling the manual rename endpoint
// enforces). Returns "" when nothing meaningful remains.
//
// Prefix-stripping, wrapper-stripping, AND trailing-punctuation trimming all
// run in a single loop until the string stops changing. They interact: a model
// may nest them (e.g. `"Title: Fix login".` or `「标题：修复登录问题」。`), where a
// trailing `.` / `。` keeps the closing wrapper from being recognized, which in
// turn hides the forbidden prefix. Iterating all three to a fixed point peels
// them in any order.
func sanitizeChatTitle(raw string) string {
	// Collapse all internal whitespace (including newlines/tabs the model may
	// emit) into single spaces so a multi-line reply becomes one clean line.
	s := strings.TrimSpace(strings.Join(strings.Fields(raw), " "))
	if s == "" {
		return ""
	}

	// Alternate stripping the leading label prefix, one layer of surrounding
	// quotes/brackets, and trailing sentence punctuation until none of them
	// changes anything. Bounded by the string only ever getting shorter, so it
	// always terminates.
	for {
		before := s
		s = strings.TrimSpace(stripChatTitleLabelPrefix(s))
		s = stripSurroundingQuotes(s)
		// ASCII + common CJK/full-width sentence punctuation. Trailing trim is
		// inside the loop so removing a trailing "." / "。" re-exposes a closing
		// wrapper (and the prefix it hides) for the next pass.
		s = strings.TrimSpace(strings.TrimRight(s, ".。!！?？,，;；:：、 "))
		if s == before || s == "" {
			break
		}
	}
	if s == "" {
		return ""
	}

	// Hard cap on rune length to match the manual-rename ceiling.
	if runes := []rune(s); len(runes) > chatSessionTitleMaxLen {
		s = strings.TrimSpace(string(runes[:chatSessionTitleMaxLen]))
	}
	return s
}

// stripChatTitleLabelPrefix removes one leading label prefix ("Title:",
// "标题：", ...) when present, matched case-insensitively. Returns s unchanged
// when none matches.
func stripChatTitleLabelPrefix(s string) string {
	lower := strings.ToLower(s)
	for _, p := range chatTitleLabelPrefixes {
		if strings.HasPrefix(lower, p) {
			return s[len(p):]
		}
	}
	return s
}

// chatTitleQuotePairs maps an opening quote/bracket to its closing partner.
var chatTitleQuotePairs = map[rune]rune{
	'"':  '"',
	'\'': '\'',
	'`':  '`',
	'“':  '”',
	'‘':  '’',
	'「':  '」',
	'『':  '』',
	'《':  '》',
	'（':  '）',
	'(':  ')',
	'【':  '】',
	'[':  ']',
}

// stripSurroundingQuotes removes matching opening/closing quote or bracket
// pairs that wrap the whole string, peeling repeatedly for nested wrappers.
func stripSurroundingQuotes(s string) string {
	for {
		runes := []rune(s)
		if len(runes) < 2 {
			return s
		}
		closer, ok := chatTitleQuotePairs[runes[0]]
		if !ok || runes[len(runes)-1] != closer {
			return s
		}
		s = strings.TrimSpace(string(runes[1 : len(runes)-1]))
		if s == "" {
			return s
		}
	}
}
