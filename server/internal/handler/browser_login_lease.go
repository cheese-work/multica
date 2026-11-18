package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/browserlease"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Class H login lease API (CHE-5989, W3-3b). Member auth: the caller is the member who holds the
// lease. A daemon credential, an agent task token and another member are refused. The mint,
// queue, revalidation and token code is the browserlease.Service of the daemon route; it serves
// class H on the C00 instance only, so `aud` carries the C00 host.

// browserLoginMember authenticates the call: a human member of the workspace.
func (h *Handler) browserLoginMember(w http.ResponseWriter, r *http.Request) (ws, user pgtype.UUID, ok bool) {
	if h.BrowserLeases == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "browser_unavailable", "browser leases are not configured")
		return ws, user, false
	}
	if middleware.DaemonWorkspaceIDFromContext(r.Context()) != "" {
		writeErrorCode(w, http.StatusForbidden, "member_required", "a login lease needs a member, not a daemon credential")
		return ws, user, false
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return ws, user, false
	}
	wsID := workspaceIDFromURL(r, "id")
	if actorType, _ := h.resolveActor(r, userID, wsID); isGovernedFieldMachineActor(r, actorType) {
		writeErrorCode(w, http.StatusForbidden, "member_required", "only a person can hold a login lease")
		return ws, user, false
	}
	if ws, ok = parseUUIDOrBadRequest(w, wsID, "workspace id"); !ok {
		return ws, user, false
	}
	if _, err := h.getWorkspaceMember(r.Context(), userID, wsID); err != nil {
		writeErrorCode(w, http.StatusForbidden, "member_required", "not a member of this workspace")
		return ws, user, false
	}
	user, ok = parseUUIDOrBadRequest(w, userID, "user id")
	return ws, user, ok
}

// ownLoginLease loads the lease and refuses one that is not a class H lease of this member.
func (h *Handler) ownLoginLease(w http.ResponseWriter, r *http.Request, ws, user pgtype.UUID) (db.BrowserLease, bool) {
	leaseID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "leaseId"), "lease_id")
	if !ok {
		return db.BrowserLease{}, false
	}
	l, err := h.Queries.GetBrowserLease(r.Context(), db.GetBrowserLeaseParams{ID: leaseID, WorkspaceID: ws})
	if errors.Is(err, pgx.ErrNoRows) {
		writeBrowserLeaseError(w, browserlease.ErrNotFound)
		return db.BrowserLease{}, false
	}
	if err != nil {
		slog.Warn("load browser login lease failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "browser lease failed")
		return db.BrowserLease{}, false
	}
	if l.Class != browserlease.ClassH || l.UserID != user {
		writeErrorCode(w, http.StatusForbidden, "not_your_lease", "this is not your login lease")
		return db.BrowserLease{}, false
	}
	return l, true
}

// MintBrowserLoginLease — POST /api/workspaces/{id}/browser-login-leases
func (h *Handler) MintBrowserLoginLease(w http.ResponseWriter, r *http.Request) {
	ws, user, ok := h.browserLoginMember(w, r)
	if !ok {
		return
	}
	var req mintBrowserLeaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	profileID, ok := parseUUIDOrBadRequest(w, req.ProfileID, "profile_id")
	if !ok {
		return
	}
	var decisionID pgtype.UUID
	if req.DecisionID != "" {
		if decisionID, ok = parseUUIDOrBadRequest(w, req.DecisionID, "decision_id"); !ok {
			return
		}
	}
	res, err := h.BrowserLeases.MintMember(r.Context(), browserlease.MemberMintRequest{
		WorkspaceID: ws, UserID: user, ProfileID: profileID, DecisionID: decisionID,
	})
	if err != nil {
		writeBrowserLeaseError(w, err)
		return
	}
	status := http.StatusCreated
	if res.Lease.State == "queued" {
		status = http.StatusAccepted
	}
	writeJSON(w, status, browserLeaseResponseOf(res))
}

// RenewBrowserLoginLease — POST /api/workspaces/{id}/browser-login-leases/{leaseId}/renew
func (h *Handler) RenewBrowserLoginLease(w http.ResponseWriter, r *http.Request) {
	ws, user, ok := h.browserLoginMember(w, r)
	if !ok {
		return
	}
	l, ok := h.ownLoginLease(w, r, ws, user)
	if !ok {
		return
	}
	res, err := h.BrowserLeases.RenewMember(r.Context(), ws, user, l.ID)
	if err != nil {
		writeBrowserLeaseError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, browserLeaseResponseOf(res))
}

// ReleaseBrowserLoginLease — POST /api/workspaces/{id}/browser-login-leases/{leaseId}/release
func (h *Handler) ReleaseBrowserLoginLease(w http.ResponseWriter, r *http.Request) {
	ws, user, ok := h.browserLoginMember(w, r)
	if !ok {
		return
	}
	l, ok := h.ownLoginLease(w, r, ws, user)
	if !ok {
		return
	}
	revoked, err := h.browserLeaseRevoker().Revoke(r.Context(), browserlease.Filter{
		WorkspaceID: ws, LeaseID: l.ID, UserID: user,
	}, browserlease.ReasonRelease)
	if err != nil {
		slog.Warn("release browser login lease failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to release the browser lease")
		return
	}
	writeJSON(w, http.StatusOK, browserLeaseRevokeResponse{Revoked: len(revoked)})
}
