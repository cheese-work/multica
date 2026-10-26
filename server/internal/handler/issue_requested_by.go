package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/logger"
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
		isMember, err := h.isWorkspaceMember(r.Context(), uuidToString(id), workspaceID)
		if err != nil {
			slog.Error("create issue: look up requester", append(logger.RequestAttrs(r), "error", err)...)
			writeError(w, http.StatusInternalServerError, "failed to look up requested_by_id")
			return pgtype.UUID{}, false
		}
		if !isMember {
			writeError(w, http.StatusBadRequest, "requested_by_id must be a member of this workspace")
			return pgtype.UUID{}, false
		}
		return id, true
	}
	candidate := h.invokeOriginatorFromRequest(r, actorType, actorID)
	if candidate == "" {
		return pgtype.UUID{}, true
	}
	isMember, err := h.isWorkspaceMember(r.Context(), candidate, workspaceID)
	if err != nil {
		slog.Error("create issue: look up requester", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to look up the requester")
		return pgtype.UUID{}, false
	}
	if !isMember {
		return pgtype.UUID{}, true
	}
	return parseUUID(candidate), true
}

// isWorkspaceMember separates "not a member" from a failed lookup.
func (h *Handler) isWorkspaceMember(ctx context.Context, userID, workspaceID string) (bool, error) {
	_, err := h.getWorkspaceMember(ctx, userID, workspaceID)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	}
	return false, err
}

// previewRequester is the requester a create preview assumes: the acting
// member. An agent's run originator is only known on the real create.
func previewRequester(actorType, actorID string) pgtype.UUID {
	if actorType == "member" {
		return parseUUID(actorID)
	}
	return pgtype.UUID{}
}
