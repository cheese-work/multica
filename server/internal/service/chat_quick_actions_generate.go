package service

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
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Quick actions are a bg:quick_actions background job (CHE-4013): a pass queues
// one output-only task on the workspace's quick_actions chain, and the completion
// writer attaches the sanitised result to the turn's assistant message. With no
// agent configured there is no task and no pills.

// Context window for the pass. Suggestions are about where the conversation
// goes next, so only the tail matters — older turns cost tokens and latency
// without changing the answer.
const (
	chatQuickActionsContextMessages = 6
	// The latest assistant reply gets the largest share: it is what the
	// suggestions must be anchored in.
	chatQuickActionsLatestBudget = 3000
	chatQuickActionsOlderBudget  = 800
	// Head/tail split applied to an over-long latest reply. The tail carries
	// the conclusion and the proposed next steps — exactly the material
	// suggestions are built from — so keeping only the head would strip the
	// most useful part.
	chatQuickActionsHeadBudget = 2000
	chatQuickActionsTailBudget = 1000
	// Cap on how many previously-suggested labels are replayed to the model.
	chatQuickActionsPreviousMax = 6
)

// chatQuickActionsMaxConcurrent bounds how many quick_actions tasks may be
// active at once, across the deployment. Passes over the ceiling are dropped, not
// queued: a suggestion that arrives after the client's pending window has
// expired is worth nothing, so shedding beats backlogging.
const chatQuickActionsMaxConcurrent = 16

// ChatQuickActionsOrigin says which caller asked for a pass. It decides how a
// failure is reported: an explicit refresh is a user action that must surface
// its own failure, while the automatic pass is best-effort background work the
// user never asked for and must fail silently (protocol.ChatQuickActionsPayload
// documents Failed as false for the automatic pass, and the client turns every
// failed=true into a "couldn't refresh" toast).
type ChatQuickActionsOrigin int

const (
	ChatQuickActionsAutomatic ChatQuickActionsOrigin = iota
	ChatQuickActionsRefresh
)

// chatQuickActionsSystemPrompt is the entire instruction set for the pass. It
// is a system prompt (stable across calls, so upstream prompt caching applies);
// the per-call conversation goes in the user message.
//
// The rules are deliberately prescriptive about WHO the suggestions are for.
// The retired daemon pass ran as a resumed turn inside the agent's own session,
// so it inherited the Multica runtime brief's identity and drifted toward
// agent-operations actions; this pass has no such context and must be told the
// frame explicitly.
//
// Output language is settled by chatQuickActionsLanguageRule, which closes the
// user message; this prompt only says that rule wins. Naming a specific
// language in these stable instructions is what this file must avoid — a model
// reading "…in Chinese" in a rule about button width will sometimes take it as
// permission to answer in Chinese (MUL-5689).
//
// The word "JSON" must stay in this text: response_format=json_object is
// rejected upstream without it.
const chatQuickActionsSystemPrompt = `You generate follow-up suggestions for a chat between a user and an AI agent.
Your output is rendered as three clickable buttons under the agent's latest
reply. Clicking one sends that suggestion's "prompt" to the same agent as the
user's next message.

You write FOR THE USER, not for the agent. Every suggestion must be something
the user would plausibly want to ask or do next — never a task the agent should
perform on its own, never a status report, never a question addressed to the user.

Quality bar:
- Return exactly 3 suggestions.
- Every suggestion must be anchored in something concrete the latest agent reply
  actually mentioned — a file, a name, an option it listed, a caveat it raised,
  a next step it proposed. Never invent a topic the conversation did not touch.
- Never suggest something the agent already did in this turn.
- Never repeat or paraphrase anything under ALREADY SUGGESTED.
- Make the three distinct from each other: different intents, not three
  rewordings of the same request.

Field rules:
- "label": the button text. A short verb phrase — at most 6 words, or at most 12
  characters in a script that does not put spaces between words. No trailing
  punctuation, no quotes, no emoji. It is a button, not a sentence.
- "prompt": the full message sent on the user's behalf. First person, the user's
  own voice, and SELF-CONTAINED — the agent never sees the label, so the prompt
  must carry every detail itself. One or two sentences.
- "primary": true on exactly one suggestion, the single most likely next step.
  false on all others.

Language: the user message contains a LANGUAGE RULE line near the end. It is
authoritative; follow it exactly.

Output JSON only, exactly this shape:
{"actions":[{"label":"...","prompt":"...","primary":true}]}
No prose, no markdown, no code fences.`

// chatQuickActionsLanguageRule closes the pass's user message and is the whole
// language policy. It is last on purpose: everything above it is conversation
// that may be in a language the pills must NOT be written in, and the rule has
// to be the final thing read before the task line.
//
// It anchors on the MOST RECENT user turn, not on the window as a whole. A pill
// sends a message as the user, so it follows how they are writing right now —
// which also means switching language takes effect on the very next turn
// instead of waiting for the window to tip.
//
// Everything else is named and excluded explicitly, because each one has been
// observed to pull the output the wrong way (MUL-5689): the agent may reply in
// another language, the system prompt is English, and ALREADY SUGGESTED
// replays the previous turn's labels — which is what made one bad pass stick,
// each Chinese label seeding the next round.
//
// Deliberately NOT done here: resolving the language server-side from
// `"user".language` or from Unicode script detection. The stored preference is
// the UI locale, not the language of this conversation, so an English UI would
// silently override someone chatting in Chinese. Script detection fares worse —
// it cannot tell one Latin-script language from another (so the model still has
// to infer), it reads an earlier Korean turn over the latest Japanese one, and
// a kana sentence carrying enough English identifiers classifies as Latin, at
// which point a "never emit CJK" clause forbids the user's own script.
const chatQuickActionsLanguageRule = `LANGUAGE RULE: Write every "label" and "prompt" in the same language as the most recent [user] message above. Ignore the agent's reply, older messages, the system instructions, and ALREADY SUGGESTED when choosing the language. If there is no [user] message, use the latest [agent] message.`

// chatQuickActionsEnqueueTimeout bounds the gate queries and the enqueue; the
// agent's run has its own deadline from the job catalog.
const chatQuickActionsEnqueueTimeout = 10 * time.Second

// quickActionsResolution is the quick_actions chain for a session's workspace.
// Only the workspace chain exists so far; the per-user chain arrives with
// CHE-4111. An empty Chain means no agent: no task and no pills.
func (s *TaskService) quickActionsResolution(ctx context.Context, cs db.ChatSession) bgagents.Resolution {
	ws, err := s.Queries.GetWorkspace(ctx, cs.WorkspaceID)
	if err != nil {
		return bgagents.Resolution{Source: bgagents.NoAgent}
	}
	var settings struct {
		BackgroundAgents json.RawMessage `json:"background_agents"`
	}
	if json.Unmarshal(ws.Settings, &settings) != nil || len(settings.BackgroundAgents) == 0 {
		return bgagents.Resolution{Source: bgagents.NoAgent}
	}
	cfg, err := bgagents.Parse(settings.BackgroundAgents, bgagents.WorkspaceScope)
	if err != nil {
		return bgagents.Resolution{Source: bgagents.NoAgent}
	}
	return bgagents.Resolve(bgagents.QuickActions, nil, cfg, func(_ bgagents.Source, agentID string) bool {
		return s.backgroundAgentReachable(ctx, bgagents.QuickActions, cs.WorkspaceID, agentID)
	})
}

// GenerateChatQuickActionsAsync queues one quick_actions task for a completed
// chat turn. The reply is already delivered and the daemon does the work, so the
// caller never waits on it. The pass is dropped (placeholder resolved, no task)
// when the deployment is at the concurrency ceiling or the session already has a
// pass active. origin decides failure reporting: an explicit refresh surfaces
// failed=true so the user who pressed the button learns it did not work, while
// the automatic pass resolves the placeholder quietly.
func (s *TaskService) GenerateChatQuickActionsAsync(task db.AgentTaskQueue, origin ChatQuickActionsOrigin) {
	ctx, cancel := context.WithTimeout(context.Background(), chatQuickActionsEnqueueTimeout)
	defer cancel()
	refresh := origin == ChatQuickActionsRefresh
	queued, failed, err := s.enqueueChatQuickActions(ctx, task, refresh)
	if err != nil {
		slog.Warn("chat quick actions enqueue failed", "task_id", util.UUIDToString(task.ID), "error", err)
	}
	if !queued {
		s.resolveChatQuickActionsPlaceholder(ctx, task, failed && refresh)
	}
}

// enqueueChatQuickActions reports whether a task was queued. failed marks an
// outcome the user would call a failure (no usable agent, an error) as opposed to
// a deliberate drop.
func (s *TaskService) enqueueChatQuickActions(ctx context.Context, task db.AgentTaskQueue, refresh bool) (queued, failed bool, err error) {
	if !task.ChatSessionID.Valid {
		return false, false, nil
	}
	// Resolve the target turn FIRST and anchor everything to it: by the time the
	// agent answers, the session may already have moved on by a full turn.
	target, err := s.Queries.GetChatMessageByTaskAssistant(ctx, task.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil // no assistant row (no_response, channel empty-drop): no client waits
	}
	if err != nil {
		return false, true, fmt.Errorf("load chat quick actions target: %w", err)
	}
	if target.MessageKind != protocol.ChatMessageKindMessage || strings.TrimSpace(target.Content) == "" {
		return false, false, nil
	}
	cs, err := s.Queries.GetChatSession(ctx, task.ChatSessionID)
	if err != nil {
		return false, true, fmt.Errorf("load chat session: %w", err)
	}
	res := s.quickActionsResolution(ctx, cs)
	if len(res.Chain) == 0 {
		return false, true, nil
	}
	prompt, err := s.buildChatQuickActionsPrompt(ctx, target, chatInputOwnerID(task))
	if err != nil {
		return false, true, err
	}
	in := BackgroundTaskInput{
		Job: bgagents.QuickActions, WorkspaceID: cs.WorkspaceID, MemberID: cs.CreatorID, SourceChatSessionID: cs.ID,
		Source: res.Source,
		// The daemon builds an output-only prompt from the input alone, so the
		// instructions travel with the conversation.
		Input:        chatQuickActionsSystemPrompt + "\n\n" + prompt,
		TargetTaskID: task.ID, Refresh: refresh,
	}
	return s.admitQuickActions(ctx, in, res.Chain)
}

// admitQuickActions queues the pass through the chain. The two admission gates
// run in the insert transaction (BackgroundTaskInput.Admit) under a
// transaction-scoped advisory lock, so across every server instance a burst
// cannot exceed the ceiling or queue duplicates for one DM. The lock is taken
// last, after every pool read, so a waiting admission holds no pool demand.
func (s *TaskService) admitQuickActions(ctx context.Context, in BackgroundTaskInput, chain []bgagents.Candidate) (queued, failed bool, err error) {
	purpose := pgtype.Text{String: bgagents.Purpose(bgagents.QuickActions), Valid: true}
	in.Admit = func(ctx context.Context, tx db.DBTX, q *db.Queries) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", purpose.String); err != nil {
			return fmt.Errorf("lock quick actions admission: %w", err)
		}
		active, err := q.HasActiveBackgroundTaskForSession(ctx, db.HasActiveBackgroundTaskForSessionParams{Purpose: purpose, SourceChatSessionID: in.SourceChatSessionID})
		if err != nil {
			return fmt.Errorf("check active quick actions task: %w", err)
		}
		if active {
			slog.Info("chat quick actions pass skipped: one already active for this session", "chat_session_id", util.UUIDToString(in.SourceChatSessionID))
			return ErrBackgroundDropped
		}
		count, err := q.CountActiveBackgroundTasksByPurpose(ctx, purpose)
		if err != nil {
			return fmt.Errorf("count active quick actions tasks: %w", err)
		}
		if count >= chatQuickActionsMaxConcurrent {
			slog.Warn("chat quick actions pass shed: concurrency ceiling reached", "ceiling", chatQuickActionsMaxConcurrent)
			return ErrBackgroundDropped
		}
		return nil
	}
	switch _, err := s.EnqueueBackgroundChain(ctx, in, chain); {
	case err == nil:
		return true, false, nil
	case errors.Is(err, ErrBackgroundDropped):
		return false, false, nil
	case errors.Is(err, ErrNoReachableBackgroundAgent), errors.Is(err, ErrBackgroundChainChanged):
		return false, true, nil
	default:
		return false, true, err
	}
}

// chatQuickActionsInFlight reports whether a quick_actions task is already
// active for this session. The refresh endpoint checks it so a duplicate request
// is refused with a 409 the client can act on, instead of being accepted and
// dropped by the gate in enqueueChatQuickActions.
func (s *TaskService) chatQuickActionsInFlight(ctx context.Context, sessionID pgtype.UUID) (bool, error) {
	return s.Queries.HasActiveBackgroundTaskForSession(ctx, db.HasActiveBackgroundTaskForSessionParams{
		Purpose: pgtype.Text{String: bgagents.Purpose(bgagents.QuickActions), Valid: true}, SourceChatSessionID: sessionID,
	})
}

// quickActionsTarget reads back the chat turn a quick_actions task attaches to.
func quickActionsTarget(task db.AgentTaskQueue) (BackgroundTaskContext, pgtype.UUID, bool) {
	var c BackgroundTaskContext
	if json.Unmarshal(task.Context, &c) != nil {
		return c, pgtype.UUID{}, false
	}
	target, err := util.ParseUUID(c.TargetTaskID)
	return c, target, err == nil
}

// writeQuickActionsOutput is the bg:quick_actions completion writer. It runs in
// the completion transaction: an output the sanitiser rejects writes nothing and
// does not fail over (ErrInvalidBackgroundOutput).
func writeQuickActionsOutput(ctx context.Context, q *db.Queries, task db.AgentTaskQueue, result []byte) error {
	_, target, ok := quickActionsTarget(task)
	if !ok {
		return ErrInvalidBackgroundOutput
	}
	actions := cleanChatQuickActions(backgroundOutputText(result))
	if len(actions) == 0 {
		return ErrInvalidBackgroundOutput
	}
	encoded, err := json.Marshal(actions)
	if err != nil {
		return fmt.Errorf("marshal chat quick actions: %w", err)
	}
	if _, err := q.SetChatMessageQuickActionsByTask(ctx, db.SetChatMessageQuickActionsByTaskParams{TaskID: target, QuickActions: encoded}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidBackgroundOutput
		}
		return fmt.Errorf("attach chat quick actions: %w", err)
	}
	return nil
}

// resolveQuickActionsTask ends the client's pending skeleton for a finished or
// ended quick_actions task by rebroadcasting the turn's current pills. failed
// marks an explicit refresh that produced nothing. Used after the completion
// commit and by the no-agent path.
func (s *TaskService) resolveQuickActionsTask(ctx context.Context, task db.AgentTaskQueue, written bool) {
	c, target, ok := quickActionsTarget(task)
	if !ok || !task.SourceChatSessionID.Valid {
		return
	}
	// The turn's own session is the bg task's source session: SupplementChatQuickActions
	// reads and broadcasts under task.ChatSessionID.
	turn := db.AgentTaskQueue{ID: target, ChatSessionID: task.SourceChatSessionID}
	s.resolveChatQuickActionsPlaceholder(ctx, turn, c.Refresh && !written)
}

// resolveChatQuickActionsPlaceholder ends a client's pending skeleton without
// generating anything, rebroadcasting the turn's current pills.
func (s *TaskService) resolveChatQuickActionsPlaceholder(ctx context.Context, task db.AgentTaskQueue, failed bool) {
	if err := s.SupplementChatQuickActions(ctx, task, "", failed); err != nil {
		slog.Warn("chat quick actions placeholder resolve failed",
			"task_id", util.UUIDToString(task.ID), "error", err)
	}
}

// buildChatQuickActionsPrompt renders the pass's user message from the window
// of conversation ENDING AT target, which is the assistant turn the suggestions
// will be attached to.
//
// Anchoring on the target (rather than re-reading the session's newest
// messages) is what keeps an async pass correct: a turn that lands between the
// completion callback and this read would otherwise supply the context while
// the result is still written to the older turn, and a newly-sent user message
// would leave the window ending on a user row with no reply to build on.
func (s *TaskService) buildChatQuickActionsPrompt(ctx context.Context, target db.ChatMessage, inputOwnerID pgtype.UUID) (string, error) {
	// Strictly older than target, newest-first; reversed below. Over-fetch so
	// dropped rows (no_response, failures) don't shrink the window below the
	// intended turn count.
	rows, err := s.Queries.ListChatMessagesPage(ctx, db.ListChatMessagesPageParams{
		ChatSessionID:   target.ChatSessionID,
		Limit:           chatQuickActionsContextMessages * 2,
		BeforeCreatedAt: target.CreatedAt,
		BeforeID:        target.ID,
	})
	if err != nil {
		return "", fmt.Errorf("load chat messages for quick actions: %w", err)
	}
	msgs := selectChatQuickActionsContext(rows, target, inputOwnerID)
	return renderChatQuickActionsContext(msgs, collectPreviousChatQuickActions(msgs)), nil
}

func selectChatQuickActionsContext(rows []db.ChatMessage, target db.ChatMessage, inputOwnerID pgtype.UUID) []db.ChatMessage {
	// A queued successor's input can be stored before target even though it is
	// logically the next turn. Keep user rows only when their task has already
	// answered inside this anchored window (or owns the target task's input;
	// auto-retry replies use the child task ID while their input keeps the root).
	answeredTaskIDs := map[pgtype.UUID]struct{}{target.TaskID: {}}
	answeredTaskIDs[inputOwnerID] = struct{}{}
	for _, msg := range rows {
		if msg.Role == "assistant" && msg.TaskID.Valid {
			answeredTaskIDs[msg.TaskID] = struct{}{}
		}
	}
	msgs := make([]db.ChatMessage, 0, len(rows)+1)
	for i := len(rows) - 1; i >= 0; i-- {
		msg := rows[i]
		if msg.Role == "user" && msg.TaskID.Valid {
			if _, answered := answeredTaskIDs[msg.TaskID]; !answered {
				continue
			}
		}
		// Only ordinary turns carry usable text. A no_response row holds an
		// English placeholder body and a failure row holds an error, neither of
		// which describes what the conversation is about.
		if msg.MessageKind != protocol.ChatMessageKindMessage {
			continue
		}
		if strings.TrimSpace(msg.Content) == "" {
			continue
		}
		msgs = append(msgs, msg)
	}
	if len(msgs) > chatQuickActionsContextMessages-1 {
		msgs = msgs[len(msgs)-(chatQuickActionsContextMessages-1):]
	}
	msgs = append(msgs, target)
	return msgs
}

// collectPreviousChatQuickActions gathers the labels already offered in this
// window so the prompt can forbid repeating them. The newest assistant row is
// included: on an explicit refresh it holds the pills the user is asking to
// replace, and offering the same three back is the one outcome a refresh must
// never produce.
//
// These labels are a de-duplication list ONLY. They are replayed verbatim, so
// on a session that once drifted they are the wrong language — which is exactly
// how one bad pass used to seed the next. chatQuickActionsLanguageRule names
// them explicitly as something to ignore when choosing the output language.
func collectPreviousChatQuickActions(msgs []db.ChatMessage) []string {
	labels := make([]string, 0, chatQuickActionsPreviousMax)
	seen := make(map[string]struct{}, chatQuickActionsPreviousMax)
	// Newest first, so the most recent suggestions survive the cap.
	for i := len(msgs) - 1; i >= 0; i-- {
		if len(msgs[i].QuickActions) == 0 {
			continue
		}
		var actions []protocol.ChatQuickAction
		if err := json.Unmarshal(msgs[i].QuickActions, &actions); err != nil {
			continue
		}
		for _, action := range actions {
			label := strings.TrimSpace(action.Label)
			if label == "" {
				continue
			}
			key := strings.ToLower(label)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			labels = append(labels, label)
			if len(labels) == chatQuickActionsPreviousMax {
				return labels
			}
		}
	}
	return labels
}

// renderChatQuickActionsContext formats the conversation window and the
// already-suggested labels into the pass's user message. Pure, so the
// truncation rules are unit-testable without a database.
//
// msgs is oldest-first and its last entry must be the assistant reply the
// suggestions are for.
func renderChatQuickActionsContext(msgs []db.ChatMessage, previous []string) string {
	var b strings.Builder
	b.WriteString("CONVERSATION (oldest first):\n")
	for i, msg := range msgs {
		speaker := "agent"
		if msg.Role == "user" {
			speaker = "user"
		}
		content := strings.TrimSpace(msg.Content)
		if i == len(msgs)-1 {
			content = truncateChatQuickActionsLatest(content)
		} else {
			content = truncateChatQuickActionsRunes(content, chatQuickActionsOlderBudget)
		}
		fmt.Fprintf(&b, "[%s]: %s\n", speaker, content)
	}

	b.WriteString("\nALREADY SUGGESTED (do not repeat or paraphrase):\n")
	if len(previous) == 0 {
		b.WriteString("(none)\n")
	} else {
		for _, label := range previous {
			fmt.Fprintf(&b, "- %s\n", label)
		}
	}

	b.WriteString("\n")
	b.WriteString(chatQuickActionsLanguageRule)

	b.WriteString("\n\nProduce the follow-up suggestions for the latest agent reply.")
	return b.String()
}

// truncateChatQuickActionsLatest shortens the anchor reply while keeping both
// ends. The head establishes what the reply is about and the tail holds its
// conclusion and proposed next steps; a plain head-only cut on a long reply
// discards exactly the material the suggestions should be built from.
func truncateChatQuickActionsLatest(content string) string {
	runes := []rune(content)
	if len(runes) <= chatQuickActionsLatestBudget {
		return content
	}
	head := string(runes[:chatQuickActionsHeadBudget])
	tail := string(runes[len(runes)-chatQuickActionsTailBudget:])
	return head + "\n…[truncated]…\n" + tail
}

// truncateChatQuickActionsRunes cuts an older message to a rune budget. Head
// only: for context turns, what the message opened with is enough to follow the
// thread.
func truncateChatQuickActionsRunes(content string, maxRunes int) string {
	runes := []rune(content)
	if len(runes) <= maxRunes {
		return content
	}
	return string(runes[:maxRunes]) + "…"
}
