package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
)

// chatWakeupTaskID returns the chat task the request runs as. The outcome route
// is an agent-facing surface: the task token names the DM, so no session id is
// passed. (Fork helper; upstream's wakeup handler resolves the turn with
// chatTurnTask instead.)
func chatWakeupTaskID(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	if r.Header.Get("X-Actor-Source") != "task_token" {
		writeError(w, http.StatusForbidden, "only a running chat task can report an outcome")
		return pgtype.UUID{}, false
	}
	taskID, err := util.ParseUUID(r.Header.Get("X-Task-ID"))
	if err != nil {
		writeError(w, http.StatusForbidden, "a running chat task is required")
		return pgtype.UUID{}, false
	}
	return taskID, true
}

// PostChatOutcome is the agent-facing end-of-turn report (plan r4 T6-T8). The task token names the
// DM and the turn, so no session id is accepted.
func (h *Handler) PostChatOutcome(w http.ResponseWriter, r *http.Request) {
	taskID, ok := chatWakeupTaskID(w, r)
	if !ok {
		return
	}
	var req struct {
		Kind   string   `json:"kind"`
		Ask    string   `json:"ask"`
		New    bool     `json:"new"`
		AskIDs []string `json:"ask_ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ids, err := h.TaskService.RecordChatOutcome(r.Context(), taskID, service.ChatOutcomeInput{
		Kind: req.Kind, Ask: req.Ask, New: req.New, AskIDs: req.AskIDs,
	})
	switch {
	case err == nil:
	case errors.Is(err, service.ErrChatOutcomeInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, service.ErrChatOutcomeTaskUnknown):
		writeError(w, http.StatusNotFound, err.Error())
		return
	case errors.Is(err, service.ErrChatOutcomeNotChat):
		writeError(w, http.StatusForbidden, err.Error())
		return
	case errors.Is(err, service.ErrChatOutcomeTaskEnded):
		writeError(w, http.StatusConflict, err.Error())
		return
	default:
		writeError(w, http.StatusInternalServerError, "chat outcome failed")
		return
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = uuidToString(id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ask_ids": out})
}
