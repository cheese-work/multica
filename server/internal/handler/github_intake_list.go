package handler

import (
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type githubIntakeDelivery struct {
	Event          string `json:"event"`
	DeliveryGUID   string `json:"delivery_guid"`
	State          string `json:"state"`
	Reason         string `json:"reason"`
	ReceivedAt     string `json:"received_at"`
	RevisionState  string `json:"revision_state"`
	RevisionReason string `json:"revision_reason"`
	Tasks          int32  `json:"tasks"`
}

// ListGitHubIntake (GET /api/github/intake?since=<RFC3339>&limit=<1-500>) reads
// delivery metadata for the notify-only check (CHE-2669). Read-only; payload and
// signature are never selected.
func (h *Handler) ListGitHubIntake(w http.ResponseWriter, r *http.Request) {
	since, err := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "since is required (RFC3339)")
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if limit, err = strconv.Atoi(raw); err != nil || limit < 1 || limit > 500 {
			writeError(w, http.StatusBadRequest, "limit must be 1-500")
			return
		}
	}
	rows, err := h.Queries.ListGitHubWebhookIntakeSince(r.Context(), db.ListGitHubWebhookIntakeSinceParams{
		WorkspaceID: parseUUID(h.resolveWorkspaceID(r)),
		ReceivedAt:  pgtype.Timestamptz{Time: since, Valid: true},
		Limit:       int32(limit),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read intake")
		return
	}
	out := make([]githubIntakeDelivery, 0, len(rows))
	for _, row := range rows {
		out = append(out, githubIntakeDelivery{
			Event: row.Event, DeliveryGUID: row.DeliveryGuid, State: row.State, Reason: row.Reason,
			ReceivedAt: timestampToString(row.ReceivedAt), RevisionState: row.RevisionState,
			RevisionReason: row.RevisionReason, Tasks: row.Tasks,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": out})
}
