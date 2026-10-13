package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"github.com/multica-ai/multica/server/pkg/redact"
)

type ChatContinueResponse struct {
	Session           ChatSessionResponse `json:"session"`
	PreviousSessionID string              `json:"previous_session_id"`
	URL               string              `json:"url"`
}

func (handler *Handler) ForkChatSession(writer http.ResponseWriter, request *http.Request) {
	handler.createSummaryChatSession(writer, request, true)
}

func (h *Handler) ContinueChatSession(w http.ResponseWriter, r *http.Request) {
	h.createSummaryChatSession(w, r, false)
}

func (h *Handler) createSummaryChatSession(w http.ResponseWriter, r *http.Request, fork bool) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := ctxWorkspaceID(r.Context())
	sessionID := chi.URLParam(r, "sessionId")
	var session db.ChatSession
	operationKey := ""
	if r.Header.Get("X-Actor-Source") == "task_token" {
		taskID, err := util.ParseUUID(r.Header.Get("X-Task-ID"))
		if err != nil {
			writeError(w, http.StatusForbidden, "a session chat task is required")
			return
		}
		task, err := h.Queries.GetAgentTask(r.Context(), taskID)
		if err != nil || !task.ChatSessionID.Valid || (task.Status != "running" && task.Status != "dispatched") {
			writeError(w, http.StatusForbidden, "a running session chat task is required")
			return
		}
		if sessionID != "" && sessionID != uuidToString(task.ChatSessionID) {
			writeError(w, http.StatusForbidden, "task does not belong to this chat session")
			return
		}
		session, err = h.Queries.GetChatSession(r.Context(), task.ChatSessionID)
		if err != nil || uuidToString(session.WorkspaceID) != workspaceID || session.AgentID != task.AgentID {
			writeError(w, http.StatusForbidden, "task does not belong to this chat session")
			return
		}
		operationKey = uuidToString(task.ID)
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
	var request struct {
		Summary        string `json:"summary"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || strings.TrimSpace(request.Summary) == "" || len(request.Summary) > 64<<10 {
		writeError(w, http.StatusBadRequest, "a non-empty summary of at most 65536 bytes is required")
		return
	}
	if fork {
		if request.IdempotencyKey != "" {
			operationKey = request.IdempotencyKey
		}
		if strings.TrimSpace(operationKey) == "" || len(operationKey) > 128 || !utf8.ValidString(operationKey) || strings.ContainsRune(operationKey, '\x00') {
			writeError(w, http.StatusBadRequest, "an idempotency_key of at most 128 bytes is required")
			return
		}
		if !utf8.ValidString(request.Summary) || strings.ContainsRune(request.Summary, '\x00') {
			writeError(w, http.StatusBadRequest, "summary must be UTF-8 without NUL characters")
			return
		}
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to start continuation")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	if _, err := qtx.LockWorkspaceForChatSessionCreate(r.Context(), session.WorkspaceID); err != nil {
		writeError(w, http.StatusNotFound, "workspace not found")
		return
	}
	if session.ProjectID.Valid {
		if _, err := qtx.LockProjectForChatSessionCreate(r.Context(), db.LockProjectForChatSessionCreateParams{ID: session.ProjectID, WorkspaceID: session.WorkspaceID}); err != nil {
			writeError(w, http.StatusConflict, "the source project is no longer available")
			return
		}
	}
	session, err = qtx.LockChatSessionForEnqueue(r.Context(), session.ID)
	if err != nil {
		writeError(w, http.StatusNotFound, "chat session not found")
		return
	}
	workspace, err := qtx.GetWorkspace(r.Context(), session.WorkspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load workspace")
		return
	}
	chatURL := func(id pgtype.UUID) string {
		return "/" + url.PathEscape(workspace.Slug) + "/chat?session=" + uuidToString(id)
	}
	if fork {
		next, err := qtx.GetChatForkByOperation(r.Context(), db.GetChatForkByOperationParams{
			ForkedFromSessionID: session.ID, ForkIdempotencyKey: pgtype.Text{String: operationKey, Valid: true},
		})
		if err == nil {
			if next.CreatorID != session.CreatorID || next.WorkspaceID != session.WorkspaceID {
				writeError(w, http.StatusConflict, "the fork is no longer available")
				return
			}
			writeJSON(w, http.StatusOK, ChatContinueResponse{Session: chatSessionToResponse(next), PreviousSessionID: uuidToString(session.ID), URL: chatURL(next.ID)})
			return
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusInternalServerError, "failed to check fork operation")
			return
		}
	} else if session.ContinuedIntoSessionID.Valid {
		next, err := qtx.GetChatSessionInWorkspace(r.Context(), db.GetChatSessionInWorkspaceParams{ID: session.ContinuedIntoSessionID, WorkspaceID: session.WorkspaceID})
		if err != nil || next.CreatorID != session.CreatorID {
			writeError(w, http.StatusConflict, "the continuation is no longer available")
			return
		}
		writeJSON(w, http.StatusOK, ChatContinueResponse{Session: chatSessionToResponse(next), PreviousSessionID: uuidToString(session.ID), URL: chatURL(next.ID)})
		return
	}
	if session.Status != "active" {
		writeError(w, http.StatusConflict, "only an active DM can be continued")
		return
	}
	if fork && session.ChannelOriginAt.Valid {
		writeError(w, http.StatusConflict, "fork is only available for web DMs")
		return
	}
	if _, err := qtx.GetChannelChatSessionBindingBySessionAny(r.Context(), session.ID); !errors.Is(err, pgx.ErrNoRows) {
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to check DM source")
		} else {
			writeError(w, http.StatusConflict, "continue is only available for web DMs")
		}
		return
	}
	title := session.Title
	if fork {
		title += " (fork)"
	}
	next, err := qtx.CreateChatSession(r.Context(), db.CreateChatSessionParams{
		ID: dbid.NewV7(), WorkspaceID: session.WorkspaceID, AgentID: session.AgentID,
		CreatorID: session.CreatorID, Title: title, ProjectID: session.ProjectID,
	})
	var summary db.ChatMessage
	if err == nil {
		summary, err = qtx.CreateChatMessage(r.Context(), db.CreateChatMessageParams{
			ID: dbid.NewV7(), ChatSessionID: next.ID, Role: "assistant",
			Content:     strings.TrimSpace(redact.Text(request.Summary)) + fmt.Sprintf("\n\n[Previous DM](%s)", chatURL(session.ID)),
			MessageKind: pgtype.Text{String: protocol.ChatMessageKindContinuationSummary, Valid: true},
		})
	}
	if err == nil && fork {
		next, err = qtx.SetChatForkSource(r.Context(), db.SetChatForkSourceParams{
			ID: next.ID, ForkedFromSessionID: session.ID,
			ForkIdempotencyKey: pgtype.Text{String: operationKey, Valid: true}, SummaryCreatedAt: summary.CreatedAt,
		})
	} else if err == nil {
		next, err = qtx.SetChatContinuationSource(r.Context(), db.SetChatContinuationSourceParams{
			ID: next.ID, ContinuedFromSessionID: session.ID, SummaryCreatedAt: summary.CreatedAt, PinnedAt: session.PinnedAt,
		})
	}
	if err == nil && !fork {
		session, err = qtx.ArchiveContinuedChatSession(r.Context(), db.ArchiveContinuedChatSessionParams{ID: session.ID, ContinuedIntoSessionID: next.ID})
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to continue chat session")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to commit continuation")
		return
	}
	creatorID := uuidToString(session.CreatorID)
	h.publishChat(protocol.EventChatSessionCreated, workspaceID, "member", creatorID, uuidToString(next.ID), protocol.ChatSessionCreatedPayload{
		WorkspaceID: workspaceID, ChatSessionID: uuidToString(next.ID), AgentID: uuidToString(next.AgentID), CreatorID: creatorID, Title: next.Title,
	})
	if !fork {
		status := session.Status
		h.publishChat(protocol.EventChatSessionUpdated, workspaceID, "member", creatorID, uuidToString(session.ID), protocol.ChatSessionUpdatedPayload{
			ChatSessionID: uuidToString(session.ID), Title: session.Title, Status: &status,
			UpdatedAt: timestampToString(session.UpdatedAt), ContinuedIntoSessionID: uuidToString(next.ID),
		})
	}
	writeJSON(w, http.StatusCreated, ChatContinueResponse{Session: chatSessionToResponse(next), PreviousSessionID: uuidToString(session.ID), URL: chatURL(next.ID)})
}
