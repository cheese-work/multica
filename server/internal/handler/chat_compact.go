package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"github.com/multica-ai/multica/server/pkg/redact"
)

type ChatCompactResponse struct {
	Session          ChatSessionResponse `json:"session"`
	SummaryMessageID string              `json:"summary_message_id"`
}

// CompactChatSession compacts a DM in place: the running chat task stores its
// summary in the same DM and opens a new compaction epoch. The DM is neither
// replaced nor archived. Only the DM's own running task may call it.
func (h *Handler) CompactChatSession(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Actor-Source") != "task_token" {
		writeError(w, http.StatusForbidden, "only the DM's running chat task can compact")
		return
	}
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
	workspaceID := ctxWorkspaceID(r.Context())
	session, err := h.Queries.GetChatSession(r.Context(), task.ChatSessionID)
	if err != nil || uuidToString(session.WorkspaceID) != workspaceID || session.AgentID != task.AgentID {
		writeError(w, http.StatusForbidden, "task does not belong to this chat session")
		return
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
		writeError(w, http.StatusInternalServerError, "failed to start compaction")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	session, err = qtx.LockChatSessionForEnqueue(r.Context(), session.ID)
	if err != nil {
		writeError(w, http.StatusNotFound, "chat session not found")
		return
	}
	if session.Status != "active" {
		writeError(w, http.StatusConflict, "only an active DM can be compacted")
		return
	}
	// The status read at the top is stale by now: the task may have been
	// cancelled or completed while the body decoded and the DM lock was awaited,
	// and a successor may already hold the old epoch. Lock the live task (DM
	// first, then task) and read its status again.
	task, err = qtx.LockAgentTaskForCompaction(r.Context(), task.ID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "55P03" { // lock_not_available
			writeError(w, http.StatusConflict, "the task is changing state; retry if it is still running")
		} else {
			writeError(w, http.StatusInternalServerError, "failed to lock the compacting task")
		}
		return
	}
	if task.ChatSessionID != session.ID || (task.Status != "running" && task.Status != "dispatched") {
		writeError(w, http.StatusConflict, "the task is no longer running")
		return
	}
	if _, err := qtx.GetChannelChatSessionBindingBySessionAny(r.Context(), session.ID); !errors.Is(err, pgx.ErrNoRows) {
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to check DM source")
		} else {
			writeError(w, http.StatusConflict, "compact is only available for web DMs")
		}
		return
	}
	// A retry from the task that already wrote the summary changes nothing.
	summary, err := qtx.GetTaskCompactionSummary(r.Context(), db.GetTaskCompactionSummaryParams{ChatSessionID: session.ID, TaskID: task.ID})
	if err == nil {
		writeJSON(w, http.StatusOK, ChatCompactResponse{Session: chatSessionToResponse(session), SummaryMessageID: uuidToString(summary.ID)})
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "failed to check existing summary")
		return
	}
	summary, err = qtx.CreateChatMessage(r.Context(), db.CreateChatMessageParams{
		ID: dbid.NewV7(), ChatSessionID: session.ID, Role: "assistant",
		Content:     strings.TrimSpace(redact.Prose(request.Summary)),
		TaskID:      task.ID,
		MessageKind: pgtype.Text{String: protocol.ChatMessageKindCompactionSummary, Valid: true},
	})
	if err == nil {
		session, err = qtx.CompactChatSessionInPlace(r.Context(), session.ID)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to compact chat session")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to commit compaction")
		return
	}
	sessionID := uuidToString(session.ID)
	h.publishChat(protocol.EventChatMessage, workspaceID, "agent", uuidToString(task.AgentID), sessionID, protocol.ChatMessagePayload{
		ChatSessionID: sessionID, MessageID: uuidToString(summary.ID), Role: summary.Role, Content: summary.Content,
		TaskID: uuidToString(task.ID), CreatedAt: timestampToString(summary.CreatedAt),
	})
	writeJSON(w, http.StatusOK, ChatCompactResponse{Session: chatSessionToResponse(session), SummaryMessageID: uuidToString(summary.ID)})
}
