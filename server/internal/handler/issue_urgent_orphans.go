package handler

import (
	"net/http"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ListUrgentOrphanIssues returns urgent issues that no task or wakeup will
// pick up. The pulse reads it so an urgent issue never sits unowned.
func (h *Handler) ListUrgentOrphanIssues(w http.ResponseWriter, r *http.Request) {
	wsUUID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace_id")
	if !ok {
		return
	}
	terminal, err := h.terminalIssueStatusKeys(r.Context(), wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to resolve status categories")
		return
	}
	rows, err := h.Queries.ListUrgentOrphanIssues(r.Context(), db.ListUrgentOrphanIssuesParams{
		WorkspaceID:        wsUUID,
		TerminalStatusKeys: terminal,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list urgent orphan issues")
		return
	}
	type entry struct {
		ID           string  `json:"id"`
		Number       int32   `json:"number"`
		Title        string  `json:"title"`
		Status       string  `json:"status"`
		AssigneeType *string `json:"assignee_type"`
		AssigneeID   *string `json:"assignee_id"`
		UpdatedAt    string  `json:"updated_at"`
	}
	resp := make([]entry, len(rows))
	for i, row := range rows {
		resp[i] = entry{
			ID:           uuidToString(row.ID),
			Number:       row.Number,
			Title:        row.Title,
			Status:       row.Status,
			AssigneeType: textToPtr(row.AssigneeType),
			AssigneeID:   uuidToPtr(row.AssigneeID),
			UpdatedAt:    timestampToString(row.UpdatedAt),
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"issues": resp})
}
