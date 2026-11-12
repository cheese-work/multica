package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/service"
)

func validateProviderHoldSetting(value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = service.ProviderHoldSecondsFromSettings(raw)
	return err
}

type providerBreakerResponse struct {
	Provider      string   `json:"provider"`
	State         string   `json:"state"`
	OpenReason    string   `json:"open_reason"`
	FailureCount  int      `json:"failure_count"`
	OpenedAt      *string  `json:"opened_at"`
	NextProbeAt   *string  `json:"next_probe_at"`
	LastSuccessAt *string  `json:"last_success_at"`
	TripEvidence  []string `json:"trip_evidence"`
}

// ListProviderBreakers returns every provider breaker of the workspace.
func (h *Handler) ListProviderBreakers(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if _, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found"); !ok {
		return
	}
	rows, err := h.Queries.ListProviderBreakers(r.Context(), parseUUID(workspaceID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list provider breakers")
		return
	}
	out := make([]providerBreakerResponse, 0, len(rows))
	for _, row := range rows {
		evidence := make([]string, 0, len(row.TripEvidence))
		// A closed breaker keeps its last trip evidence; it is history, not a current failure count.
		if row.State != "closed" {
			for _, id := range row.TripEvidence {
				evidence = append(evidence, uuidToString(id))
			}
		}
		item := providerBreakerResponse{Provider: row.Provider, State: row.State, FailureCount: len(evidence), TripEvidence: evidence}
		if row.State != "closed" {
			item.OpenReason = "repeated provider 5xx failures"
		}
		if row.OpenedAt.Valid {
			s := timestampToString(row.OpenedAt)
			item.OpenedAt = &s
		}
		if row.NextProbeAt.Valid {
			s := timestampToString(row.NextProbeAt)
			item.NextProbeAt = &s
		}
		if row.LastSuccessAt.Valid {
			s := timestampToString(row.LastSuccessAt)
			item.LastSuccessAt = &s
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"breakers": out})
}

// ResetProviderBreaker force-closes a breaker. Owner/admin only; the reset and its audit row commit together.
func (h *Handler) ResetProviderBreaker(w http.ResponseWriter, r *http.Request) {
	// Human only whatever the task write-scope mode is: a machine credential bound to an admin must not reset.
	if isMachineCredentialActor(r) {
		writeError(w, http.StatusForbidden, "this endpoint is only available to human actors")
		return
	}
	workspaceID := h.resolveWorkspaceID(r)
	if _, ok := h.requireWorkspaceRole(w, r, workspaceID, "workspace not found", "owner", "admin"); !ok {
		return
	}
	provider := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "provider")))
	if provider == "" {
		writeError(w, http.StatusBadRequest, "provider is required")
		return
	}
	ctx := r.Context()
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reset provider breaker")
		return
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	previous, found, err := service.ResetProviderBreaker(ctx, h.Queries.WithTx(tx), parseUUID(workspaceID), provider, parseUUID(requestUserID(r)))
	if err != nil {
		slog.Error("provider breaker reset failed", "workspace_id", workspaceID, "provider", provider, "err", err)
		writeError(w, http.StatusInternalServerError, "failed to reset provider breaker")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "provider breaker not found")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reset provider breaker")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"provider": provider, "previous_state": previous, "state": "closed"})
}
