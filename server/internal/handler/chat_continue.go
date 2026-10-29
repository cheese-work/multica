package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/dmnotify"
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

func (h *Handler) ContinueChatSession(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := ctxWorkspaceID(r.Context())
	sessionID := chi.URLParam(r, "sessionId")
	var session db.ChatSession
	// compactReason is the server-built reason of an automatic compaction: it
	// leads the new DM's summary so the person sees why the DM moved.
	var compactReason string
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
		compactReason = task.CompactReason.String
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
		Summary string `json:"summary"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || strings.TrimSpace(request.Summary) == "" || len(request.Summary) > 64<<10 {
		writeError(w, http.StatusBadRequest, "a non-empty summary of at most 65536 bytes is required")
		return
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
	if !decisionLaneArchiveAllowed(w, r, qtx, session) {
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
	if session.ContinuedIntoSessionID.Valid {
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
	if _, err := qtx.GetChannelChatSessionBindingBySessionAny(r.Context(), session.ID); !errors.Is(err, pgx.ErrNoRows) {
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to check DM source")
		} else {
			writeError(w, http.StatusConflict, "continue is only available for web DMs")
		}
		return
	}
	next, err := qtx.CreateChatSession(r.Context(), db.CreateChatSessionParams{
		ID: dbid.NewV7(), WorkspaceID: session.WorkspaceID, AgentID: session.AgentID,
		CreatorID: session.CreatorID, Title: session.Title, ProjectID: session.ProjectID,
	})
	var summary db.ChatMessage
	if err == nil {
		summary, err = qtx.CreateChatMessage(r.Context(), db.CreateChatMessageParams{
			ID: dbid.NewV7(), ChatSessionID: next.ID, Role: "assistant",
			Content:     compactSummaryContent(compactReason, request.Summary, chatURL(session.ID)),
			MessageKind: pgtype.Text{String: protocol.ChatMessageKindContinuationSummary, Valid: true},
		})
	}
	if err == nil {
		next, err = qtx.SetChatContinuationSource(r.Context(), db.SetChatContinuationSourceParams{
			ID: next.ID, ContinuedFromSessionID: session.ID, SummaryCreatedAt: summary.CreatedAt, PinnedAt: session.PinnedAt,
		})
	}
	if err == nil {
		session, err = qtx.ArchiveContinuedChatSession(r.Context(), db.ArchiveContinuedChatSessionParams{ID: session.ID, ContinuedIntoSessionID: next.ID})
	}
	// Turns still queued in the source DM are answered in the new one.
	if err == nil {
		var movedTaskIDs []pgtype.UUID
		movedTaskIDs, err = qtx.MoveUnstartedChatTasksToSession(r.Context(), db.MoveUnstartedChatTasksToSessionParams{FromSessionID: session.ID, ToSessionID: next.ID})
		if err == nil && len(movedTaskIDs) > 0 {
			err = qtx.MoveChatMessagesOfTasksToSession(r.Context(), db.MoveChatMessagesOfTasksToSessionParams{
				FromSessionID: session.ID, ToSessionID: next.ID, TaskIds: movedTaskIDs, SummaryCreatedAt: summary.CreatedAt,
			})
		}
	}
	// An agent rollover archives the DM the run was replying in, so that reply
	// cannot notify. The summary is the agent's message: notify in the new DM.
	// A continue the DM creator submitted stays silent.
	// The archived source DM gets no more notifications, so the ones it already
	// raised close with it, whoever asked for the continuation.
	var closedRecords []db.InboxItem
	if err == nil {
		closedRecords, err = qtx.MarkDMRepliesRead(r.Context(), db.MarkDMRepliesReadParams{ChatSessionID: session.ID, RecipientID: session.CreatorID})
	}
	var dmNotified *dmnotify.Notified
	if err == nil && r.Header.Get("X-Actor-Source") == "task_token" {
		dmNotified, err = dmnotify.Reply(r.Context(), qtx, next.ID, summary)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to continue chat session")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to commit continuation")
		return
	}
	dmnotify.Publish(h.Bus, dmNotified)
	creatorID := uuidToString(session.CreatorID)
	h.publishDMRepliesRead(workspaceID, creatorID, closedRecords)
	h.publishChat(protocol.EventChatSessionCreated, workspaceID, "member", creatorID, uuidToString(next.ID), protocol.ChatSessionCreatedPayload{
		WorkspaceID: workspaceID, ChatSessionID: uuidToString(next.ID), AgentID: uuidToString(next.AgentID), CreatorID: creatorID, Title: next.Title,
	})
	status := session.Status
	pinned := session.PinnedAt.Valid
	h.publishChat(protocol.EventChatSessionUpdated, workspaceID, "member", creatorID, uuidToString(session.ID), protocol.ChatSessionUpdatedPayload{
		ChatSessionID: uuidToString(session.ID), Title: session.Title, Status: &status, Pinned: &pinned,
		UpdatedAt: timestampToString(session.UpdatedAt), ContinuedIntoSessionID: uuidToString(next.ID),
	})
	writeJSON(w, http.StatusCreated, ChatContinueResponse{Session: chatSessionToResponse(next), PreviousSessionID: uuidToString(session.ID), URL: chatURL(next.ID)})
}

// compactSummaryContent is the new DM's first message: the agent's summary and
// the link back, led by the automatic compaction's reason when there is one.
func compactSummaryContent(reason, summary, previousURL string) string {
	content := strings.TrimSpace(redact.Text(summary)) + fmt.Sprintf("\n\n[Previous DM](%s)", previousURL)
	if reason == "" {
		return content
	}
	return reason + "\n\n" + content
}
