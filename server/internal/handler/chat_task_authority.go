package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

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
			return refuseChatTask(codeCompactionContinueRefused, "A compaction task finishes in place with `multica chat compact`; it cannot continue or fork the DM.")
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

// authorizeChatTask loads the workspace setting, decides, logs the decision and
// writes the refusal. It returns true when the task may proceed.
func (h *Handler) authorizeChatTask(ctx context.Context, w http.ResponseWriter, q *db.Queries, op chatTaskOp, task db.AgentTaskQueue, session db.ChatSession, workspaceID string) bool {
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
		writeErrorCode(w, http.StatusForbidden, refusal.Code, refusal.Message)
		return false
	}
	level := slog.LevelDebug
	if in.isCompaction() {
		level = slog.LevelInfo
	}
	slog.Log(ctx, level, chatTaskAuthorityEvent, append(attrs, "decision", "granted")...)
	return true
}

// currentCompactAgent reads settings.compact_agent_id and whether that agent is
// still eligible (ENG-8) and invokable by the session creator (CEO-9(b)). A
// failed lookup counts as unusable.
func (h *Handler) currentCompactAgent(ctx context.Context, q *db.Queries, session db.ChatSession) (string, bool) {
	workspace, err := q.GetWorkspace(ctx, session.WorkspaceID)
	if err != nil {
		return "", false
	}
	var settings map[string]any
	_ = json.Unmarshal(workspace.Settings, &settings)
	id, _ := settings[compactAgentIDKey].(string)
	if id == "" {
		return "", false
	}
	if ok, err := compactAgentEligible(ctx, q, session.WorkspaceID, id); err != nil || !ok {
		return id, false
	}
	agent, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: parseUUID(id), WorkspaceID: session.WorkspaceID})
	if err != nil {
		return id, false
	}
	creator := uuidToString(session.CreatorID)
	return id, h.canInvokeAgent(ctx, agent, "member", creator, creator, uuidToString(session.WorkspaceID))
}
