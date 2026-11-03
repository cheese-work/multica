package handler

import (
	"context"
	"net/http"
)

type passkeySummary struct {
	ID         string   `json:"id"`
	CreatedAt  string   `json:"created_at"`
	LastUsedAt *string  `json:"last_used_at"`
	Transports []string `json:"transports"`
	// State is "active" or "suspect". A suspect key cannot sign but can still be revoked.
	State string `json:"state"`
}

// ListPasskeys returns the caller's unrevoked passkeys, suspect ones included, for the settings page. Like
// revocation it stays open while enrollment is closed, and it exposes no key
// material: only what a person needs to tell their devices apart.
func (handler *Handler) ListPasskeys(writer http.ResponseWriter, request *http.Request) {
	userID, ok := interactiveCaller(writer, request)
	if !ok {
		return
	}
	if handler.Passkeys == nil {
		writeFeatureDisabled(writer, "passkeys_unavailable", "passkeys are not configured")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), decisionActionDeadline)
	defer cancel()
	rows, err := handler.Queries.ListLivePasskeys(ctx, userID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "failed to list passkeys")
		return
	}
	passkeys := make([]passkeySummary, 0, len(rows))
	for _, row := range rows {
		transports := row.Transports
		if transports == nil {
			transports = []string{}
		}
		state := "active"
		if row.SuspectAt.Valid {
			state = "suspect"
		}
		passkeys = append(passkeys, passkeySummary{
			ID: uuidToString(row.ID), CreatedAt: timestampToString(row.CreatedAt),
			LastUsedAt: timestampToPtr(row.LastUsedAt), Transports: transports, State: state,
		})
	}
	writeJSON(writer, http.StatusOK, map[string][]passkeySummary{"passkeys": passkeys})
}
