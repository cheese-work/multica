package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// DM wakeups (CHE-1874). The caller is an agent's running chat task: its token
// resolves the target DM, so an agent can only wake the DM it is replying in.

type ChatWakeupResponse struct {
	ID            string  `json:"id"`
	ChatSessionID string  `json:"chat_session_id"`
	Instruction   string  `json:"instruction"`
	FireAt        string  `json:"fire_at"`
	Status        string  `json:"status"`
	TaskID        *string `json:"task_id"`
}

func chatWakeupToResponse(w db.ChatWakeup) ChatWakeupResponse {
	return ChatWakeupResponse{
		ID:            uuidToString(w.ID),
		ChatSessionID: uuidToString(w.ChatSessionID),
		Instruction:   w.Instruction,
		FireAt:        timestampToString(w.FireAt),
		Status:        w.Status,
		TaskID:        uuidToPtr(w.TaskID),
	}
}

func (h *Handler) chatWakeups() *service.ChatWakeupService {
	return &service.ChatWakeupService{Tasks: h.TaskService}
}

// chatWakeupTask resolves the calling chat task and its DM, or writes the error.
func (h *Handler) chatWakeupTask(w http.ResponseWriter, r *http.Request) (db.AgentTaskQueue, bool) {
	if r.Header.Get("X-Actor-Source") != "task_token" {
		writeError(w, http.StatusForbidden, "chat wakeups are only available from within an agent chat task")
		return db.AgentTaskQueue{}, false
	}
	taskID, err := util.ParseUUID(r.Header.Get("X-Task-ID"))
	if err != nil {
		writeError(w, http.StatusForbidden, "a session chat task is required")
		return db.AgentTaskQueue{}, false
	}
	task, err := h.Queries.GetAgentTask(r.Context(), taskID)
	if err != nil || !task.ChatSessionID.Valid || (task.Status != "running" && task.Status != "dispatched") {
		writeError(w, http.StatusForbidden, "a running session chat task is required")
		return db.AgentTaskQueue{}, false
	}
	session, err := h.Queries.GetChatSession(r.Context(), task.ChatSessionID)
	if err != nil || uuidToString(session.WorkspaceID) != ctxWorkspaceID(r.Context()) || session.AgentID != task.AgentID {
		writeError(w, http.StatusForbidden, "task does not belong to this chat session")
		return db.AgentTaskQueue{}, false
	}
	return task, true
}

func (h *Handler) CreateChatWakeup(w http.ResponseWriter, r *http.Request) {
	task, ok := h.chatWakeupTask(w, r)
	if !ok {
		return
	}
	var request struct {
		Instruction  string     `json:"instruction"`
		At           *time.Time `json:"at"`
		AfterSeconds int64      `json:"after_seconds"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	now := time.Now()
	var fireAt time.Time
	switch {
	case (request.At == nil) == (request.AfterSeconds <= 0):
		writeError(w, http.StatusBadRequest, "provide exactly one of at or after_seconds")
		return
	case request.At != nil:
		fireAt = *request.At
	default:
		fireAt = now.Add(time.Duration(request.AfterSeconds) * time.Second)
	}
	wakeup, err := h.chatWakeups().Create(r.Context(), task.ChatSessionID, task.ID, request.Instruction, fireAt, now)
	switch {
	case errors.Is(err, service.ErrChatWakeupInput):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, service.ErrChatWakeupCap), errors.Is(err, service.ErrChatWakeupUnavailable):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, "failed to schedule wakeup")
	default:
		writeJSON(w, http.StatusCreated, chatWakeupToResponse(wakeup))
	}
}

func (h *Handler) ListChatWakeups(w http.ResponseWriter, r *http.Request) {
	task, ok := h.chatWakeupTask(w, r)
	if !ok {
		return
	}
	rows, err := h.Queries.ListPendingChatWakeups(r.Context(), task.ChatSessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list wakeups")
		return
	}
	out := make([]ChatWakeupResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, chatWakeupToResponse(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"wakeups": out})
}

func (h *Handler) CancelChatWakeup(w http.ResponseWriter, r *http.Request) {
	task, ok := h.chatWakeupTask(w, r)
	if !ok {
		return
	}
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "wakeupId"), "wakeup id")
	if !ok {
		return
	}
	wakeup, err := h.Queries.CancelChatWakeup(r.Context(), db.CancelChatWakeupParams{ID: id, ChatSessionID: task.ChatSessionID})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "no pending wakeup with this id in this DM")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to cancel wakeup")
		return
	}
	writeJSON(w, http.StatusOK, chatWakeupToResponse(wakeup))
}
