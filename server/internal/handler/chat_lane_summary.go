package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
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
	if err != nil || !task.ChatSessionID.Valid || task.Status != "running" {
		writeError(writer, http.StatusForbidden, "this task is not a chat task")
		return db.ChatSession{}, false
	}
	session, err := handler.Queries.GetChatSession(request.Context(), task.ChatSessionID)
	if err != nil || uuidToString(session.WorkspaceID) != ctxWorkspaceID(request.Context()) || session.AgentID != task.AgentID {
		writeError(writer, http.StatusForbidden, "chat task does not belong to this session and workspace")
		return db.ChatSession{}, false
	}
	if target := chi.URLParam(request, "sessionId"); target != "" && target != uuidToString(session.ID) {
		writeError(writer, http.StatusForbidden, "chat task cannot access another session")
		return db.ChatSession{}, false
	}
	return session, true
}

func (handler *Handler) SetChatLaneSummary(writer http.ResponseWriter, request *http.Request) {
	userID, ok := requireUserID(writer, request)
	if !ok {
		return
	}
	var session db.ChatSession
	if request.Header.Get("X-Actor-Source") == "interactive_jwt" {
		session, ok = handler.gatePublicChatSessionForUser(writer, request, userID, ctxWorkspaceID(request.Context()), chi.URLParam(request, "sessionId"))
	} else {
		session, ok = handler.callingChatSession(writer, request)
	}
	if !ok {
		return
	}
	var body struct {
		LaneSummary *string `json:"lane_summary"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 16*1024)).Decode(&body); err != nil || body.LaneSummary == nil {
		writeError(writer, http.StatusBadRequest, "lane_summary must be a string")
		return
	}
	if strings.ContainsRune(*body.LaneSummary, '\x00') {
		writeError(writer, http.StatusBadRequest, "lane_summary must not contain NUL characters")
		return
	}
	if utf8.RuneCountInString(*body.LaneSummary) > 600 {
		writeError(writer, http.StatusBadRequest, "lane_summary must not exceed 600 Unicode characters")
		return
	}
	updated, err := handler.Queries.UpdateChatSessionLaneSummary(request.Context(), db.UpdateChatSessionLaneSummaryParams{
		ID: session.ID, WorkspaceID: session.WorkspaceID, LaneSummary: *body.LaneSummary,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(writer, http.StatusConflict, "archived chat sessions are read-only")
		} else {
			writeError(writer, http.StatusInternalServerError, "failed to update lane summary")
		}
		return
	}
	writeJSON(writer, http.StatusOK, chatSessionToResponse(updated))
}

func chatSessionReadResponse(request *http.Request, session db.ChatSession) ChatSessionResponse {
	response := chatSessionToResponse(session)
	if request.Header.Get("X-Actor-Source") != "interactive_jwt" {
		response.LaneSummary = ""
		response.LaneSummaryUpdatedAt = nil
	}
	return response
}

func (handler *Handler) chatReadSession(writer http.ResponseWriter, request *http.Request, userID string) (db.ChatSession, bool) {
	if request.Header.Get("X-Actor-Source") == "interactive_jwt" {
		return handler.gatePublicChatSessionForUser(writer, request, userID, ctxWorkspaceID(request.Context()), chi.URLParam(request, "sessionId"))
	}
	return handler.callingChatSession(writer, request)
}
