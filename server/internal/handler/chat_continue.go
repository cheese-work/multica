package handler

import (
	"context"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/internal/weblink"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type ChatContinueResponse struct {
	Session           ChatSessionResponse `json:"session"`
	PreviousSessionID string              `json:"previous_session_id"`
	URL               string              `json:"url"`
	WebURL            string              `json:"web_url,omitempty"`
}

func (h *Handler) ContinueChatSession(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := ctxWorkspaceID(r.Context())
	sessionID := chi.URLParam(r, "sessionId")
	var session db.ChatSession
	var taskID pgtype.UUID
	var task db.AgentTaskQueue
	op := chatOpContinue
	if r.Header.Get("X-Actor-Source") == "task_token" {
		headerTaskID, err := util.ParseUUID(r.Header.Get("X-Task-ID"))
		if err != nil {
			writeError(w, http.StatusForbidden, "a session chat task is required")
			return
		}
		task, err = h.Queries.GetAgentTask(r.Context(), headerTaskID)
		if err != nil || !task.ChatSessionID.Valid {
			writeErrorCode(w, http.StatusForbidden, codeChatTaskNotActive, "a running session chat task is required")
			return
		}
		if sessionID != "" && sessionID != uuidToString(task.ChatSessionID) {
			writeErrorCode(w, http.StatusForbidden, codeChatTaskSessionMismatch, "task does not belong to this chat session")
			return
		}
		session, err = h.Queries.GetChatSession(r.Context(), task.ChatSessionID)
		if err != nil {
			writeErrorCode(w, http.StatusForbidden, codeChatTaskSessionMismatch, "task does not belong to this chat session")
			return
		}
		// A compaction task finishes in place, so it is judged as a compact;
		// any other task continues the DM. The archived-DM branch below
		// refuses a compaction task outright.
		if task.Purpose.Valid && task.Purpose.String == taskPurposeCompaction {
			op = chatOpCompact
		}
		if !h.authorizeChatTask(r.Context(), w, h.Queries, op, task, session, workspaceID) {
			return
		}
		taskID = task.ID
	} else {
		if r.Header.Get("X-Agent-ID") != "" {
			writeError(w, http.StatusForbidden, "only the session human or its chat task can continue")
			return
		}
		session, ok = h.gateChatSessionForUser(w, r, userID, workspaceID, sessionID)
		if !ok {
			return
		}
	}
	// A DM archived by an earlier continuation answers with its successor.
	if session.ContinuedIntoSessionID.Valid {
		if taskID.Valid && op == chatOpCompact && !h.authorizeChatTask(r.Context(), w, h.Queries, chatOpContinue, task, session, workspaceID) {
			return
		}
		next, err := h.Queries.GetChatSessionInWorkspace(r.Context(), db.GetChatSessionInWorkspaceParams{ID: session.ContinuedIntoSessionID, WorkspaceID: session.WorkspaceID})
		if err != nil || next.CreatorID != session.CreatorID {
			writeError(w, http.StatusConflict, "the continuation is no longer available")
			return
		}
		h.writeChatContinue(r.Context(), w, session.ID, next)
		return
	}
	summaryText, ok := decodeCompactSummary(w, r)
	if !ok {
		return
	}
	session, _, fail := h.compactChatInPlace(r.Context(), session.ID, taskID, summaryText)
	if fail != nil {
		fail.write(w)
		return
	}
	h.writeChatContinue(r.Context(), w, session.ID, session)
}

// writeChatContinue answers with the DM that carries on and the one it
// continues from; both are the same DM after an in-place compact.
func (h *Handler) writeChatContinue(ctx context.Context, w http.ResponseWriter, previousID pgtype.UUID, current db.ChatSession) {
	workspace, err := h.Queries.GetWorkspace(ctx, current.WorkspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load workspace")
		return
	}
	chatURL := "/" + url.PathEscape(workspace.Slug) + "/chat?session=" + uuidToString(current.ID)
	writeJSON(w, http.StatusOK, ChatContinueResponse{Session: chatSessionToResponse(current), PreviousSessionID: uuidToString(previousID), URL: chatURL, WebURL: weblink.Build(h.cfg.AppURL, chatURL)})
}
