package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/logger"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Browser permission admin API (CHE-4737, W3-8; plan CHE-4695 W1 r3, F3). A browser lease needs
// both: the task's accountable user holds permission for the profile and the agent is on the
// profile's allowlist. Default deny: a profile with no row grants nothing. Cheese (workspace
// owner or admin, a person) administers both lists. The plan allows no egress exception, so
// nothing here stores or accepts one. W3-9 enforces the lists at mint, renew and queued grant.

type browserProfilePermissionsResponse struct {
	ProfileID string   `json:"profile_id"`
	Members   []string `json:"members"`
	Agents    []string `json:"agents"`
}

// browserPermissionRequest authorizes the caller and resolves the profile. It writes the error
// response and returns false when the caller may not administer permissions or the profile is
// not in the workspace.
func (h *Handler) browserPermissionRequest(w http.ResponseWriter, r *http.Request) (ws, profile, actor pgtype.UUID, ok bool) {
	wsID := workspaceIDFromURL(r, "id")
	if ws, ok = parseUUIDOrBadRequest(w, wsID, "workspace id"); !ok {
		return
	}
	userID, ok2 := requireUserID(w, r)
	if !ok2 {
		return ws, profile, actor, false
	}
	if actorType, _ := h.resolveActor(r, userID, wsID); isGovernedFieldMachineActor(r, actorType) {
		writeError(w, http.StatusForbidden, "only a person can administer browser permissions")
		return ws, profile, actor, false
	}
	if _, ok = h.requireWorkspaceRole(w, r, wsID, "workspace not found", "owner", "admin"); !ok {
		return
	}
	if actor, ok = parseUUIDOrBadRequest(w, userID, "user id"); !ok {
		return
	}
	if profile, ok = parseUUIDOrBadRequest(w, chi.URLParam(r, "profileId"), "profile id"); !ok {
		return
	}
	if _, err := h.Queries.GetBrowserProfile(r.Context(), db.GetBrowserProfileParams{ID: profile, WorkspaceID: ws}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "browser profile not found")
		} else {
			slog.Warn("load browser profile failed", append(logger.RequestAttrs(r), "error", err)...)
			writeError(w, http.StatusInternalServerError, "failed to load the browser profile")
		}
		return ws, profile, actor, false
	}
	return ws, profile, actor, true
}

func (h *Handler) browserPermissionFailed(w http.ResponseWriter, r *http.Request, what string, err error) {
	slog.Warn(what+" failed", append(logger.RequestAttrs(r), "error", err)...)
	writeError(w, http.StatusInternalServerError, "failed to "+what)
}

// GetBrowserProfilePermissions — GET /api/workspaces/{id}/browser-profiles/{profileId}/permissions
func (h *Handler) GetBrowserProfilePermissions(w http.ResponseWriter, r *http.Request) {
	ws, profile, _, ok := h.browserPermissionRequest(w, r)
	if !ok {
		return
	}
	members, err := h.Queries.ListBrowserProfileMembers(r.Context(), db.ListBrowserProfileMembersParams{WorkspaceID: ws, ProfileID: profile})
	if err != nil {
		h.browserPermissionFailed(w, r, "list the browser profile members", err)
		return
	}
	agents, err := h.Queries.ListBrowserProfileAgents(r.Context(), db.ListBrowserProfileAgentsParams{WorkspaceID: ws, ProfileID: profile})
	if err != nil {
		h.browserPermissionFailed(w, r, "list the browser profile agents", err)
		return
	}
	out := browserProfilePermissionsResponse{ProfileID: uuidToString(profile), Members: []string{}, Agents: []string{}}
	for _, u := range members {
		out.Members = append(out.Members, uuidToString(u))
	}
	for _, a := range agents {
		out.Agents = append(out.Agents, uuidToString(a))
	}
	writeJSON(w, http.StatusOK, out)
}

// GrantBrowserProfileMember — PUT /api/workspaces/{id}/browser-profiles/{profileId}/members/{userId}
// Idempotent. The user must be a member of the workspace.
func (h *Handler) GrantBrowserProfileMember(w http.ResponseWriter, r *http.Request) {
	ws, profile, actor, ok := h.browserPermissionRequest(w, r)
	if !ok {
		return
	}
	user, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "userId"), "user id")
	if !ok {
		return
	}
	if _, err := h.getWorkspaceMember(r.Context(), uuidToString(user), uuidToString(ws)); err != nil {
		writeError(w, http.StatusNotFound, "workspace member not found")
		return
	}
	if err := h.Queries.GrantBrowserProfileMember(r.Context(), db.GrantBrowserProfileMemberParams{
		WorkspaceID: ws, ProfileID: profile, UserID: user, GrantedBy: actor,
	}); err != nil {
		h.browserPermissionFailed(w, r, "grant the browser profile member", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RevokeBrowserProfileMember — DELETE /api/workspaces/{id}/browser-profiles/{profileId}/members/{userId}
// Idempotent.
func (h *Handler) RevokeBrowserProfileMember(w http.ResponseWriter, r *http.Request) {
	ws, profile, _, ok := h.browserPermissionRequest(w, r)
	if !ok {
		return
	}
	user, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "userId"), "user id")
	if !ok {
		return
	}
	if err := h.Queries.RevokeBrowserProfileMember(r.Context(), db.RevokeBrowserProfileMemberParams{WorkspaceID: ws, ProfileID: profile, UserID: user}); err != nil {
		h.browserPermissionFailed(w, r, "revoke the browser profile member", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GrantBrowserProfileAgent — PUT /api/workspaces/{id}/browser-profiles/{profileId}/agents/{agentId}
// Idempotent. The agent must belong to the workspace.
func (h *Handler) GrantBrowserProfileAgent(w http.ResponseWriter, r *http.Request) {
	ws, profile, actor, ok := h.browserPermissionRequest(w, r)
	if !ok {
		return
	}
	agent, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "agentId"), "agent id")
	if !ok {
		return
	}
	if _, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{ID: agent, WorkspaceID: ws}); err != nil {
		writeError(w, http.StatusNotFound, "agent not found")
		return
	}
	if err := h.Queries.GrantBrowserProfileAgent(r.Context(), db.GrantBrowserProfileAgentParams{
		WorkspaceID: ws, ProfileID: profile, AgentID: agent, GrantedBy: actor,
	}); err != nil {
		h.browserPermissionFailed(w, r, "grant the browser profile agent", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RevokeBrowserProfileAgent — DELETE /api/workspaces/{id}/browser-profiles/{profileId}/agents/{agentId}
// Idempotent.
func (h *Handler) RevokeBrowserProfileAgent(w http.ResponseWriter, r *http.Request) {
	ws, profile, _, ok := h.browserPermissionRequest(w, r)
	if !ok {
		return
	}
	agent, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "agentId"), "agent id")
	if !ok {
		return
	}
	if err := h.Queries.RevokeBrowserProfileAgent(r.Context(), db.RevokeBrowserProfileAgentParams{WorkspaceID: ws, ProfileID: profile, AgentID: agent}); err != nil {
		h.browserPermissionFailed(w, r, "revoke the browser profile agent", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
