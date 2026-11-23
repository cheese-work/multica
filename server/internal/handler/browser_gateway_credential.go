package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/browserlease"
	"github.com/multica-ai/multica/server/internal/logger"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Per-gateway credential routes (CHE-8883, A1b of plan CHE-8829 r16). Owner-only and
// human-only (the router group requires the owner role and RequireHumanActor): an agent never
// issues or revokes the credential that gates its own host. The plaintext is in the issue
// response and nowhere else; only its hash is stored. Neither route logs it.

type browserGatewayCredentialResponse struct {
	ID         string `json:"id"`
	Gateway    string `json:"gateway"`
	Credential string `json:"credential"`
	CreatedAt  string `json:"created_at"`
}

func (h *Handler) browserCredentials(w http.ResponseWriter, r *http.Request) (*browserlease.CredentialService, bool) {
	if h.BrowserLeases == nil || h.BrowserLeases.Pool == nil {
		writeError(w, http.StatusServiceUnavailable, "browser leases are not configured")
		return nil, false
	}
	return &browserlease.CredentialService{Pool: h.BrowserLeases.Pool, Audit: h.auditBrowserGatewayCredential(r)}, true
}

// auditBrowserGatewayCredential writes the audit row in the issue or revoke transaction. The row
// names the gateway and the credential id; the plaintext and the hash never reach it.
func (h *Handler) auditBrowserGatewayCredential(r *http.Request) func(context.Context, *db.Queries, string, db.BrowserGatewayCredential) error {
	return func(ctx context.Context, q *db.Queries, kind string, c db.BrowserGatewayCredential) error {
		in := auditInput{Kind: kind, WorkspaceID: c.WorkspaceID, TargetID: c.ID}
		fields := map[string]any{"gateway": c.Gateway, "workspace": uuidToString(c.WorkspaceID)}
		if kind == browserlease.AuditCredentialRevoke {
			in.Before, in.After = map[string]any{"revoked": false}, map[string]any{"revoked": true}
			for k, v := range fields {
				in.Before[k], in.After[k] = v, v
			}
		} else {
			in.After = fields
		}
		if err := h.writeAudit(ctx, q, r, in); err != nil {
			return auditWriteError{err}
		}
		return nil
	}
}

// IssueBrowserGatewayCredential — POST /api/workspaces/{id}/browser-gateways/{gateway}/credentials
func (h *Handler) IssueBrowserGatewayCredential(w http.ResponseWriter, r *http.Request) {
	svc, ok := h.browserCredentials(w, r)
	if !ok {
		return
	}
	ws, ok := parseUUIDOrBadRequest(w, workspaceIDFromURL(r, "id"), "workspace id")
	if !ok {
		return
	}
	issued, err := svc.Issue(r.Context(), ws, chi.URLParam(r, "gateway"))
	switch {
	case errors.Is(err, browserlease.ErrUnknownGateway):
		writeError(w, http.StatusNotFound, "unknown gateway")
		return
	case failedAudit(w, r, err, browserlease.AuditCredentialIssue):
		return
	case err != nil:
		slog.Warn("issue browser gateway credential failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to issue the gateway credential")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, browserGatewayCredentialResponse{
		ID: uuidToString(issued.ID), Gateway: issued.Gateway, Credential: issued.Plaintext,
		CreatedAt: issued.CreatedAt.UTC().Format(time.RFC3339Nano),
	})
}

// RevokeBrowserGatewayCredential — DELETE /api/workspaces/{id}/browser-gateways/{gateway}/credentials/{credentialId}
// Idempotent. A credential of another workspace or gateway is 404.
func (h *Handler) RevokeBrowserGatewayCredential(w http.ResponseWriter, r *http.Request) {
	svc, ok := h.browserCredentials(w, r)
	if !ok {
		return
	}
	ws, ok := parseUUIDOrBadRequest(w, workspaceIDFromURL(r, "id"), "workspace id")
	if !ok {
		return
	}
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "credentialId"), "credential id")
	if !ok {
		return
	}
	err := svc.Revoke(r.Context(), ws, chi.URLParam(r, "gateway"), id)
	switch {
	case errors.Is(err, browserlease.ErrUnknownGateway), errors.Is(err, browserlease.ErrCredentialNotFound):
		writeError(w, http.StatusNotFound, "credential not found")
		return
	case failedAudit(w, r, err, browserlease.AuditCredentialRevoke):
		return
	case err != nil:
		slog.Warn("revoke browser gateway credential failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to revoke the gateway credential")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
