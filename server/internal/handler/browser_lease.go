package handler

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/browserlease"
	"github.com/multica-ai/multica/server/internal/logger"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// SteelBrowserPluginKey is the manifest key of the plugin that turns Steel leases on
// (plugins/steel-browser, CHE-4730). Disabling or uninstalling it revokes every lease.
const SteelBrowserPluginKey = "work.cheese.steel-browser"

// browserLeaseRevoker is cheap: two pointers, no state.
func (h *Handler) browserLeaseRevoker() *browserlease.Revoker {
	return &browserlease.Revoker{Queries: h.Queries, Bus: h.Bus}
}

type browserLeaseRevokeResponse struct {
	Revoked int `json:"revoked"`
}

// ReleaseBrowserLease — POST /api/daemon/tasks/{id}/browser-leases/{leaseId}/release
//
// The agent's own task ends one of its leases. Only the task that holds the lease can release
// it; a lease of another task, or one that already ended, revokes nothing (revoked: 0).
func (h *Handler) ReleaseBrowserLease(w http.ResponseWriter, r *http.Request) {
	task, workspaceID, ok := h.requireDaemonTaskAccessWithWorkspace(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	workspaceUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}
	leaseUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "leaseId"), "lease_id")
	if !ok {
		return
	}
	revoked, err := h.browserLeaseRevoker().Revoke(r.Context(), browserlease.Filter{
		WorkspaceID: workspaceUUID,
		LeaseID:     leaseUUID,
		TaskID:      task.ID,
	}, browserlease.ReasonRelease)
	if err != nil {
		slog.Warn("release browser lease failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to release the browser lease")
		return
	}
	writeJSON(w, http.StatusOK, browserLeaseRevokeResponse{Revoked: len(revoked)})
}

// RevokeAllBrowserLeases — POST /api/workspaces/{id}/browser-leases/revoke-all
//
// Cheese's kill switch: ends every queued and active lease of the workspace. Owner or admin,
// a person only (the same gate as the scheduling pause).
func (h *Handler) RevokeAllBrowserLeases(w http.ResponseWriter, r *http.Request) {
	wsID := workspaceIDFromURL(r, "id")
	wsUUID, ok := parseUUIDOrBadRequest(w, wsID, "workspace id")
	if !ok {
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	if actorType, _ := h.resolveActor(r, userID, wsID); isGovernedFieldMachineActor(r, actorType) {
		writeError(w, http.StatusForbidden, "only a person can revoke every browser lease")
		return
	}
	if _, ok := h.requireWorkspaceRole(w, r, wsID, "workspace not found", "owner", "admin"); !ok {
		return
	}
	revoked, err := h.browserLeaseRevoker().Revoke(r.Context(), browserlease.Filter{WorkspaceID: wsUUID}, browserlease.ReasonRevokeAll)
	if err != nil {
		slog.Warn("revoke all browser leases failed", append(logger.RequestAttrs(r), "error", err, "workspace_id", wsID)...)
		writeError(w, http.StatusInternalServerError, "failed to revoke the browser leases")
		return
	}
	writeJSON(w, http.StatusOK, browserLeaseRevokeResponse{Revoked: len(revoked)})
}

// revokeLeasesOfDisabledPlugin ends the workspace's leases once the Steel browser plugin is
// off. The plugin change has committed already, so a failure here is logged, never returned:
// the caller must not report the disable as failed. The lease expiry (token 15 min, hard limit
// 2 h) and the gateway's stream check (5 s) bound the exposure.
func (h *Handler) revokeLeasesOfDisabledPlugin(r *http.Request, installation db.PluginInstallation) {
	if installation.PluginKey != SteelBrowserPluginKey {
		return
	}
	filter := browserlease.Filter{WorkspaceID: installation.WorkspaceID}
	if _, err := h.browserLeaseRevoker().Revoke(r.Context(), filter, browserlease.ReasonPluginDisabled); err != nil {
		slog.Error("revoke browser leases of a disabled plugin failed", append(logger.RequestAttrs(r), "error", err, "workspace_id", uuidToString(installation.WorkspaceID))...)
	}
}
