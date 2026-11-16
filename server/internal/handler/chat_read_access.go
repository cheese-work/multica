package handler

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func (handler *Handler) callingChatSession(writer http.ResponseWriter, request *http.Request) (db.ChatSession, bool) {
	if request.Header.Get("X-Actor-Source") == "cli_jwt" {
		writeErrorCode(writer, http.StatusForbidden, "interactive_login_required", "Sign in again to use Chat")
		return db.ChatSession{}, false
	}
	if request.Header.Get("X-Actor-Source") != "task_token" {
		writeError(writer, http.StatusForbidden, "interactive authentication or an own-session chat task is required")
		return db.ChatSession{}, false
	}
	taskID, err := util.ParseUUID(request.Header.Get("X-Task-ID"))
	if err != nil {
		writeError(writer, http.StatusForbidden, "chat task context is required")
		return db.ChatSession{}, false
	}
	task, err := handler.Queries.GetAgentTask(request.Context(), taskID)
	if err != nil || !task.ChatSessionID.Valid {
		writeError(writer, http.StatusForbidden, "this task is not a chat task")
		return db.ChatSession{}, false
	}
	session, err := handler.Queries.GetChatSession(request.Context(), task.ChatSessionID)
	if err != nil {
		writeError(writer, http.StatusForbidden, "chat task does not belong to this session and workspace")
		return db.ChatSession{}, false
	}
	// The target check comes first so a refused target never logs a grant.
	if target := chi.URLParam(request, "sessionId"); target != "" && target != uuidToString(session.ID) {
		writeError(writer, http.StatusForbidden, "chat task cannot access another session")
		return db.ChatSession{}, false
	}
	if !handler.authorizeChatTask(request.Context(), writer, handler.Queries, chatOpRead, task, session, ctxWorkspaceID(request.Context())) {
		return db.ChatSession{}, false
	}
	return session, true
}

func (handler *Handler) chatReadSession(writer http.ResponseWriter, request *http.Request, userID string) (db.ChatSession, bool) {
	if request.Header.Get("X-Actor-Source") == "interactive_jwt" {
		// A DM can hold an issue's ask recovery notice: a person who left the
		// workspace reads none of it, even through a request that was already in flight.
		if !handler.isActiveWorkspaceMember(request.Context(), userID, ctxWorkspaceID(request.Context())) {
			writeError(writer, http.StatusForbidden, "workspace membership is required")
			return db.ChatSession{}, false
		}
		return handler.gatePublicChatSessionForUser(writer, request, userID, ctxWorkspaceID(request.Context()), chi.URLParam(request, "sessionId"))
	}
	return handler.callingChatSession(writer, request)
}

func (handler *Handler) isActiveWorkspaceMember(ctx context.Context, userID, workspaceID string) bool {
	user, userErr := util.ParseUUID(userID)
	workspace, workspaceErr := util.ParseUUID(workspaceID)
	if userErr != nil || workspaceErr != nil {
		return false
	}
	_, err := handler.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: user, WorkspaceID: workspace})
	return err == nil
}
