package handler

import (
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
)

// resolveRequestedBy picks the member a new issue is created for: the explicit
// request field, else the human at the top of the acting agent run's chain,
// else the member creating it. A named person who is not a workspace member is
// a 400. An unattributable run yields no requester, not an error.
func (h *Handler) resolveRequestedBy(w http.ResponseWriter, r *http.Request, workspaceID, actorType, actorID string, explicit *string) (pgtype.UUID, bool) {
	if explicit != nil && strings.TrimSpace(*explicit) != "" {
		id, ok := parseUUIDOrBadRequest(w, strings.TrimSpace(*explicit), "requested_by_id")
		if !ok {
			return pgtype.UUID{}, false
		}
		if _, err := h.getWorkspaceMember(r.Context(), uuidToString(id), workspaceID); err != nil {
			writeError(w, http.StatusBadRequest, "requested_by_id must be a member of this workspace")
			return pgtype.UUID{}, false
		}
		return id, true
	}
	candidate := h.invokeOriginatorFromRequest(r, actorType, actorID)
	if candidate == "" {
		return pgtype.UUID{}, true
	}
	if _, err := h.getWorkspaceMember(r.Context(), candidate, workspaceID); err != nil {
		return pgtype.UUID{}, true
	}
	return parseUUID(candidate), true
}

// previewRequester is the requester a create preview assumes: the acting
// member. An agent's run originator is only known on the real create.
func previewRequester(actorType, actorID string) pgtype.UUID {
	if actorType == "member" {
		return parseUUID(actorID)
	}
	return pgtype.UUID{}
}
