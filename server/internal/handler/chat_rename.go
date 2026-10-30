package handler

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func (handler *Handler) renameChatSession(writer http.ResponseWriter, request *http.Request, userID, rawTitle string) {
	title := strings.TrimSpace(rawTitle)
	if title == "" || len([]rune(title)) > chatSessionTitleMaxLen || strings.ContainsFunc(title, unicode.IsControl) || strings.ContainsAny(title, "\u2028\u2029") {
		writeError(writer, http.StatusBadRequest, fmt.Sprintf("title must be nonempty, single-line, and at most %d characters", chatSessionTitleMaxLen))
		return
	}
	workspaceID := ctxWorkspaceID(request.Context())
	isAgent := request.Header.Get("X-Actor-Source") == "task_token"
	var session db.ChatSession
	var ok bool
	if !isAgent {
		session, ok = handler.gatePublicChatSessionForUser(writer, request, userID, workspaceID, chi.URLParam(request, "sessionId"))
		if !ok {
			return
		}
	}
	transaction, err := handler.TxStarter.Begin(request.Context())
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "failed to start rename transaction")
		return
	}
	defer transaction.Rollback(request.Context())
	queries := handler.Queries.WithTx(transaction)
	actorType, actorID, actorName := "member", userID, ""
	if isAgent {
		actorType, actorID = "agent", request.Header.Get("X-Agent-ID")
		agentID, agentErr := util.ParseUUID(actorID)
		taskID, taskErr := util.ParseUUID(request.Header.Get("X-Task-ID"))
		if agentErr != nil || taskErr != nil {
			writeError(writer, http.StatusForbidden, "rename requires a running workspace agent task")
			return
		}
		if _, err := queries.LockDecisionCallingTask(request.Context(), db.LockDecisionCallingTaskParams{WorkspaceID: parseUUID(workspaceID), ID: taskID}); err != nil {
			writeError(writer, http.StatusForbidden, "rename requires a running workspace agent task")
			return
		}
		task, err := queries.GetAgentTask(request.Context(), taskID)
		if err != nil || task.Status != "running" || task.AgentID != agentID {
			writeError(writer, http.StatusForbidden, "rename requires a running workspace agent task")
			return
		}
		agent, err := queries.GetAgentInWorkspace(request.Context(), db.GetAgentInWorkspaceParams{ID: agentID, WorkspaceID: parseUUID(workspaceID)})
		if err != nil || agent.ArchivedAt.Valid {
			writeError(writer, http.StatusForbidden, "rename requires an active workspace agent")
			return
		}
		actorName = agent.Name
		sessionID, valid := parseUUIDOrBadRequest(writer, chi.URLParam(request, "sessionId"), "session_id")
		if !valid {
			return
		}
		session, err = queries.GetPublicChatSessionInWorkspace(request.Context(), db.GetPublicChatSessionInWorkspaceParams{ID: sessionID, WorkspaceID: agent.WorkspaceID})
		if err != nil {
			writeError(writer, http.StatusNotFound, "chat session not found")
			return
		}
	} else {
		user, err := queries.GetUser(request.Context(), parseUUID(userID))
		if err != nil {
			writeError(writer, http.StatusInternalServerError, "failed to load rename actor")
			return
		}
		actorName = user.Name
	}
	session, err = queries.LockChatSessionForEnqueue(request.Context(), session.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(writer, http.StatusNotFound, "chat session not found")
		} else {
			writeError(writer, http.StatusInternalServerError, "failed to lock chat session")
		}
		return
	}
	updated, err := queries.UpdateChatSessionTitle(request.Context(), db.UpdateChatSessionTitleParams{ID: session.ID, Title: title})
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "failed to rename chat session")
		return
	}
	message, err := queries.CreateChatMessage(request.Context(), db.CreateChatMessageParams{
		ID: dbid.NewV7(), ChatSessionID: session.ID, Role: "assistant",
		Content: fmt.Sprintf("Chat renamed: %s → %s\n\nBy [@%s](mention://%s/%s).", session.Title, title, actorName, actorType, actorID),
		SentVia: pgtype.Text{String: request.Header.Get("X-Actor-Source"), Valid: request.Header.Get("X-Actor-Source") != ""},
	})
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "failed to announce chat rename")
		return
	}
	if err := transaction.Commit(request.Context()); err != nil {
		writeError(writer, http.StatusInternalServerError, "failed to commit chat rename")
		return
	}
	sessionID := uuidToString(session.ID)
	handler.publishChatFor(protocol.EventChatSessionUpdated, workspaceID, actorType, actorID, uuidToString(session.CreatorID), sessionID, protocol.ChatSessionUpdatedPayload{ChatSessionID: sessionID, Title: updated.Title, UpdatedAt: timestampToString(updated.UpdatedAt)})
	handler.publishChatFor(protocol.EventChatMessage, workspaceID, actorType, actorID, uuidToString(session.CreatorID), sessionID, protocol.ChatMessagePayload{ChatSessionID: sessionID, MessageID: uuidToString(message.ID), Role: message.Role, Content: message.Content, CreatedAt: timestampToString(message.CreatedAt)})
	if isAgent {
		writeJSON(writer, http.StatusOK, protocol.ChatRenameResponse{ID: sessionID, Title: updated.Title, PreviousTitle: session.Title, MessageID: uuidToString(message.ID)})
		return
	}
	writeJSON(writer, http.StatusOK, chatSessionToResponse(updated))
}
