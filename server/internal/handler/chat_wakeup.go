package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type ChatWakeupResponse struct {
	ID          string `json:"id"`
	FireAt      string `json:"fire_at"`
	Instruction string `json:"instruction"`
	Status      string `json:"status"`
}

func chatWakeupToResponse(w db.ChatWakeup) ChatWakeupResponse {
	return ChatWakeupResponse{ID: uuidToString(w.ID), FireAt: timestampToString(w.FireAt), Instruction: w.Instruction, Status: w.Status}
}

// chatWakeupTaskID returns the chat task the request runs as. Wakeups are an
// agent-facing surface: the task token names the DM, so no session id is passed.
func chatWakeupTaskID(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	if r.Header.Get("X-Actor-Source") != "task_token" {
		writeError(w, http.StatusForbidden, "only a running chat task can manage DM wakeups")
		return pgtype.UUID{}, false
	}
	taskID, err := util.ParseUUID(r.Header.Get("X-Task-ID"))
	if err != nil {
		writeError(w, http.StatusForbidden, "a running chat task is required")
		return pgtype.UUID{}, false
	}
	return taskID, true
}

func (h *Handler) chatWakeupService() *service.ChatWakeupService {
	return &service.ChatWakeupService{Tasks: h.TaskService}
}

func writeChatWakeupError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrChatWakeupNotOwner):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, service.ErrChatWakeupNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, service.ErrChatWakeupLimit):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, service.ErrChatWakeupTooFar), errors.Is(err, service.ErrChatWakeupPast),
		errors.Is(err, service.ErrChatWakeupEmpty), errors.Is(err, service.ErrChatWakeupTooLong):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "chat wakeup failed")
	}
}

func (h *Handler) CreateChatWakeup(w http.ResponseWriter, r *http.Request) {
	taskID, ok := chatWakeupTaskID(w, r)
	if !ok {
		return
	}
	var req struct {
		AfterSeconds int64  `json:"after_seconds"`
		At           string `json:"at"`
		Instruction  string `json:"instruction"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var at time.Time
	switch {
	case (req.AfterSeconds > 0) == (req.At != ""):
		writeError(w, http.StatusBadRequest, "exactly one of after_seconds or at is required")
		return
	case req.AfterSeconds > int64(service.MaxChatWakeupDelay/time.Second):
		writeError(w, http.StatusBadRequest, service.ErrChatWakeupTooFar.Error())
		return
	case req.At != "":
		parsed, err := time.Parse(time.RFC3339, req.At)
		if err != nil {
			writeError(w, http.StatusBadRequest, "at must be an RFC3339 time")
			return
		}
		at = parsed
	default:
		at = time.Now().Add(time.Duration(req.AfterSeconds) * time.Second)
	}
	wakeup, err := h.chatWakeupService().Create(r.Context(), service.ChatWakeupInput{TaskID: taskID, At: at, Instruction: req.Instruction})
	if err != nil {
		writeChatWakeupError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, chatWakeupToResponse(wakeup))
}

func (h *Handler) ListChatWakeups(w http.ResponseWriter, r *http.Request) {
	taskID, ok := chatWakeupTaskID(w, r)
	if !ok {
		return
	}
	wakeups, err := h.chatWakeupService().List(r.Context(), taskID)
	if err != nil {
		writeChatWakeupError(w, err)
		return
	}
	out := make([]ChatWakeupResponse, 0, len(wakeups))
	for _, wakeup := range wakeups {
		out = append(out, chatWakeupToResponse(wakeup))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) CancelChatWakeup(w http.ResponseWriter, r *http.Request) {
	taskID, ok := chatWakeupTaskID(w, r)
	if !ok {
		return
	}
	wakeupID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "wakeupId"), "wakeupId")
	if !ok {
		return
	}
	if err := h.chatWakeupService().Cancel(r.Context(), taskID, wakeupID); err != nil {
		writeChatWakeupError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
