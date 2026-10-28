package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ClaimTaskPREvent hands the daemon the next typed PR event for a live run.
// The body carries structured fields only: no PR title, body or comment text,
// and no member author. Without the negotiated capability no row exists, so a
// legacy daemon always gets an empty claim.
func (h *Handler) ClaimTaskPREvent(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	if _, ok := h.requireDaemonTaskAccess(w, r, taskID); !ok {
		return
	}
	row, err := h.Queries.ClaimNextTaskPREvent(r.Context(), parseUUID(taskID))
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to claim PR event")
		return
	}
	body := map[string]any{
		"event_id":  uuidToString(row.EventID),
		"kind":      row.Kind,
		"pr_number": row.PrNumber,
		"head_sha":  row.HeadSha,
	}
	if row.BaseSha != "" {
		body["base_sha"] = row.BaseSha
	}
	if row.OldHeadSha != "" {
		body["old_head_sha"] = row.OldHeadSha
	}
	if row.CheckName != "" {
		body["check_name"] = row.CheckName
	}
	writeJSON(w, http.StatusOK, body)
}

func (h *Handler) AckTaskPREvent(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	if _, ok := h.requireDaemonTaskAccess(w, r, taskID); !ok {
		return
	}
	eventID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "eventId"), "event id")
	if !ok {
		return
	}
	var req ackTaskSupplementRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var err error
	if req.Delivered {
		_, err = h.Queries.AckTaskPREventDelivered(r.Context(), db.AckTaskPREventDeliveredParams{
			TaskID: parseUUID(taskID), EventID: eventID,
		})
	} else {
		_, err = h.Queries.AckTaskPREventFailed(r.Context(), db.AckTaskPREventFailedParams{
			TaskID: parseUUID(taskID), EventID: eventID,
			FailureReason: pgtype.Text{String: stableTaskSupplementFailureReason(req.Error), Valid: true},
		})
	}
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, "PR event is no longer deliverable")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to acknowledge PR event")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"event_id": uuidToString(eventID), "delivered": req.Delivered})
}
