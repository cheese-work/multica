package handler

import "net/http"

// ListWorkspaceAuditEvents is a stub (CHE-5644 red phase): an empty 200.
func (h *Handler) ListWorkspaceAuditEvents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"events": []any{}, "next_cursor": nil})
}
