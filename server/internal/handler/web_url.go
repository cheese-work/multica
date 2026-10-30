package handler

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/weblink"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// workspaceWebURL returns the absolute web URL for a workspace-scoped path, or
// "" when no app origin is configured or the workspace cannot be read. The
// lookup is skipped without an origin so unconfigured servers pay nothing.
func (h *Handler) workspaceWebURL(ctx context.Context, workspaceID pgtype.UUID, path func(ws db.Workspace) string) string {
	if h.cfg.AppURL == "" {
		return ""
	}
	ws, err := h.Queries.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return ""
	}
	return weblink.Build(h.cfg.AppURL, path(ws))
}
