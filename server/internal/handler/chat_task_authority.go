package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/multica-ai/multica/server/internal/bgagents"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Purpose-first authority for chat task access (CHE-2894 SEC-1 rules 1-3,
// ENG-2, ENG-5). The task's purpose picks the branch before any agent is
// compared, so a compaction task never gains access through agent equality
// with the DM agent.

const taskPurposeCompaction = "compaction"

// chatTaskAuthorityEvent is the structured log event of every decision.
const chatTaskAuthorityEvent = "chat_task_authority"

// Stable refusal codes (DX-4). The messages are the CLI user messages.
const (
	codeChatTaskNotActive         = "chat_task_not_active"
	codeChatTaskSessionMismatch   = "chat_task_session_mismatch"
	codeChatTaskAgentMismatch     = "chat_task_agent_mismatch"
	codeCompactionAgentNotCurrent = "compaction_agent_not_current"
	codeCompactionContinueRefused = "compaction_continue_refused"
)

type chatTaskOp int

const (
	// chatOpRead is a task-token chat read (callingChatSession).
	chatOpRead chatTaskOp = iota
	// chatOpCompact is the in-place compact endpoint.
	chatOpCompact
	// chatOpContinue is any branch that archives or forks the DM.
	chatOpContinue
)

type chatTaskRefusal struct {
	Code    string
	Message string
}

type chatTaskAuthorityInput struct {
	Op                 chatTaskOp
	Task               db.AgentTaskQueue
	Session            db.ChatSession
	RequestWorkspaceID string
	// CompactAgentID is the workspace's current compact_agent_id, "" when unset.
	CompactAgentID string
	// CompactAgentUsable: the setting's agent is eligible (ENG-8) and the
	// session creator can still invoke it. Read only for a compaction task.
	CompactAgentUsable bool
}

func (in chatTaskAuthorityInput) isCompaction() bool {
	return in.Task.Purpose.Valid && in.Task.Purpose.String == taskPurposeCompaction
}

// resolveChatTaskAuthority returns nil when the task may act on the session.
// Order: session and workspace binding (SEC-1 rule 1), then the purpose
// branch. A read of an ordinary task still needs status running; every other
// path takes dispatched or running.
func resolveChatTaskAuthority(in chatTaskAuthorityInput) *chatTaskRefusal {
	task, session := in.Task, in.Session
	if !task.ChatSessionID.Valid {
		return refuseChatTask(codeChatTaskNotActive, "This task is not a chat task.")
	}
	if task.ChatSessionID != session.ID || uuidToString(session.WorkspaceID) != in.RequestWorkspaceID {
		return refuseChatTask(codeChatTaskSessionMismatch, "This task does not belong to this chat session.")
	}
	compaction := in.isCompaction()
	active := task.Status == "running" || (task.Status == "dispatched" && (compaction || in.Op != chatOpRead))
	if !active {
		return refuseChatTask(codeChatTaskNotActive, "This task is no longer running, so it cannot use this chat session.")
	}
	if compaction {
		if in.Op == chatOpContinue {
			return refuseChatTask(codeCompactionContinueRefused, "A compaction task finishes in place with `multica chat compact` or `multica chat continue` without --fork; it cannot fork or archive the DM.")
		}
		// SEC-1 rule 2: equality with the DM agent gives no access.
		if !in.CompactAgentUsable || in.CompactAgentID == "" || uuidToString(task.AgentID) != in.CompactAgentID {
			return refuseChatTask(codeCompactionAgentNotCurrent, "This agent is no longer the workspace's compaction agent.")
		}
		return nil
	}
	if task.AgentID != session.AgentID {
		return refuseChatTask(codeChatTaskAgentMismatch, "This task's agent does not own this chat session.")
	}
	return nil
}

func refuseChatTask(code, message string) *chatTaskRefusal {
	return &chatTaskRefusal{Code: code, Message: message}
}

// authorizeChatTask decides, logs and writes the refusal. It returns true when
// the task may proceed.
func (h *Handler) authorizeChatTask(ctx context.Context, w http.ResponseWriter, q *db.Queries, op chatTaskOp, task db.AgentTaskQueue, session db.ChatSession, workspaceID string) bool {
	refusal := h.decideChatTask(ctx, q, op, task, session, workspaceID)
	if refusal != nil {
		writeErrorCode(w, http.StatusForbidden, refusal.Code, refusal.Message)
	}
	return refusal == nil
}

// decideChatTask loads the compaction setting when the task needs it, decides
// and logs the decision. A caller holding the DM lock passes its transaction's
// queries, so the decision is re-read under the lock (SEC-1 rule 5).
func (h *Handler) decideChatTask(ctx context.Context, q *db.Queries, op chatTaskOp, task db.AgentTaskQueue, session db.ChatSession, workspaceID string) *chatTaskRefusal {
	in := chatTaskAuthorityInput{Op: op, Task: task, Session: session, RequestWorkspaceID: workspaceID}
	if in.isCompaction() {
		in.CompactAgentID, in.CompactAgentUsable = h.currentCompactAgent(ctx, q, session)
	}
	refusal := resolveChatTaskAuthority(in)
	attrs := []any{
		"task_id", uuidToString(task.ID), "session_id", uuidToString(session.ID),
		"compact_agent_id", in.CompactAgentID, "purpose", task.Purpose.String,
	}
	if refusal != nil {
		slog.WarnContext(ctx, chatTaskAuthorityEvent, append(attrs, "decision", "refused", "code", refusal.Code)...)
		return refusal
	}
	level := slog.LevelDebug
	if in.isCompaction() {
		level = slog.LevelInfo
	}
	slog.Log(ctx, level, chatTaskAuthorityEvent, append(attrs, "decision", "granted")...)
	return nil
}

// currentCompactAgent is the primary agent of background_agents.compact
// (CHE-4097) and whether it is still eligible (ENG-8) and invokable by the
// session creator (CEO-9(b)). The fallback slot is a runner concern and grants
// nothing here; the retired settings.compact_agent_id is not read. A failed
// lookup counts as unusable.
func (h *Handler) currentCompactAgent(ctx context.Context, q *db.Queries, session db.ChatSession) (string, bool) {
	workspace, err := q.GetWorkspace(ctx, session.WorkspaceID)
	if err != nil {
		return "", false
	}
	var settings map[string]json.RawMessage
	_ = json.Unmarshal(workspace.Settings, &settings)
	cfg, err := bgagents.Parse(settings[backgroundAgentsKey], bgagents.WorkspaceScope)
	if err != nil {
		return "", false
	}
	id := cfg[bgagents.Compact].AgentID
	if id == "" {
		return "", false
	}
	agent, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: parseUUID(id), WorkspaceID: session.WorkspaceID})
	if err != nil {
		return id, false
	}
	resolution := bgagents.Resolve(bgagents.Compact, nil, cfg, func(_ bgagents.Source, candidate string) bool {
		return candidate == id && bgagents.WorkspaceEligible(agent)
	})
	if len(resolution.Chain) == 0 || resolution.Chain[0].Slot != bgagents.Primary {
		return id, false
	}
	// Through q, not the pool: a caller holding the DM lock is inside a
	// transaction, and a second pool connection can deadlock against it.
	creator := uuidToString(session.CreatorID)
	allowed, err := h.invokeAgentDecisionVia(ctx, q, agent, "member", creator, creator, uuidToString(session.WorkspaceID))
	return id, allowed && err == nil
}
