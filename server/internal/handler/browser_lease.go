package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/browserlease"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Browser lease API (CHE-4732, W3-3). Only a daemon credential reaches it: the agent's browser
// holds no Multica credential, and a member's session must never mint an agent lease.

type mintBrowserLeaseRequest struct {
	ProfileID  string `json:"profile_id"`
	DecisionID string `json:"decision_id,omitempty"`
}

type browserLeaseResponse struct {
	LeaseID       string     `json:"lease_id"`
	State         string     `json:"state"`
	Class         string     `json:"class"`
	InstanceID    string     `json:"instance_id,omitempty"`
	Epoch         int64      `json:"epoch,omitempty"`
	Aud           string     `json:"aud,omitempty"`
	GatewayURL    string     `json:"gateway_url,omitempty"`
	QueuePosition int64      `json:"queue_position,omitempty"`
	Token         string     `json:"token,omitempty"`
	TokenExpires  *time.Time `json:"token_expires_at,omitempty"`
	LeaseExpires  *time.Time `json:"lease_expires_at,omitempty"`
}

func browserLeaseResponseOf(res browserlease.Result) browserLeaseResponse {
	l := res.Lease
	out := browserLeaseResponse{
		LeaseID: uuidToString(l.ID), State: l.State, Class: l.Class, QueuePosition: res.QueuePosition,
	}
	if l.State == "active" {
		out.InstanceID, out.Epoch = uuidToString(l.InstanceID), l.Epoch.Int64
		exp := l.ExpiresAt.Time
		out.LeaseExpires = &exp
	}
	if res.Token != "" {
		out.Aud, out.Token = browserlease.Aud(res.Host), res.Token
		out.GatewayURL = browserGatewayURL(res.Host)
		exp := res.TokenExpires
		out.TokenExpires = &exp
	}
	return out
}

// browserGatewayURL is the operator's endpoint for a host's gateway, from
// MULTICA_BROWSER_GATEWAY_URLS (JSON object, host -> URL). Empty when unset or the host is
// missing; the token audience is a label and never becomes a URL.
func browserGatewayURL(host string) string {
	var urls map[string]string
	if json.Unmarshal([]byte(os.Getenv("MULTICA_BROWSER_GATEWAY_URLS")), &urls) != nil {
		return ""
	}
	return urls[host]
}

func writeBrowserLeaseError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, browserlease.ErrGateDenied):
		writeErrorCode(w, http.StatusForbidden, "browser_permission_denied", err.Error())
	case errors.Is(err, browserlease.ErrDecisionRequired):
		body := map[string]string{"error": err.Error(), "code": "decision_required"}
		var required browserlease.DecisionRequiredError
		if errors.As(err, &required) && required.RequestID.Valid {
			body["decision_request_id"] = uuidToString(required.RequestID)
		}
		writeJSON(w, http.StatusConflict, body)
	case errors.Is(err, browserlease.ErrDecisionInvalid):
		writeErrorCode(w, http.StatusForbidden, "decision_invalid", err.Error())
	case errors.Is(err, browserlease.ErrClass3Denied), errors.Is(err, browserlease.ErrClass3Unchecked):
		writeErrorCode(w, http.StatusForbidden, "class3_denied", err.Error())
	case errors.Is(err, browserlease.ErrClassNotMintable):
		writeErrorCode(w, http.StatusUnprocessableEntity, "class_not_mintable", err.Error())
	case errors.Is(err, browserlease.ErrLeaseExists):
		writeErrorCode(w, http.StatusConflict, "lease_exists", err.Error())
	case errors.Is(err, browserlease.ErrLeaseEnded):
		writeErrorCode(w, http.StatusConflict, "lease_ended", err.Error())
	case errors.Is(err, browserlease.ErrNotFound):
		writeErrorCode(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, browserlease.ErrNoInstance), errors.Is(err, browserlease.ErrSignerMissing):
		writeErrorCode(w, http.StatusServiceUnavailable, "browser_unavailable", err.Error())
	default:
		slog.Warn("browser lease failed", "error", err)
		writeError(w, http.StatusInternalServerError, "browser lease failed")
	}
}

// browserLeaseTask authenticates the call: a daemon credential, bound to the task's runtime.
// needRunning is set for mint and renew; release stays open after the task ends.
func (h *Handler) browserLeaseTask(w http.ResponseWriter, r *http.Request, needRunning bool) (db.AgentTaskQueue, pgtype.UUID, bool) {
	if h.BrowserLeases == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "browser_unavailable", "browser leases are not configured")
		return db.AgentTaskQueue{}, pgtype.UUID{}, false
	}
	if middleware.DaemonWorkspaceIDFromContext(r.Context()) == "" {
		writeErrorCode(w, http.StatusForbidden, "daemon_credential_required", "browser leases need a daemon credential")
		return db.AgentTaskQueue{}, pgtype.UUID{}, false
	}
	task, wsID, ok := h.requireDaemonTaskAccessWithWorkspace(w, r, chi.URLParam(r, "id"))
	if !ok {
		return db.AgentTaskQueue{}, pgtype.UUID{}, false
	}
	if needRunning && task.Status != "running" {
		writeErrorCode(w, http.StatusConflict, "task_not_running", "task is not running")
		return db.AgentTaskQueue{}, pgtype.UUID{}, false
	}
	return task, parseUUID(wsID), true
}

func (h *Handler) MintBrowserLease(w http.ResponseWriter, r *http.Request) {
	task, ws, ok := h.browserLeaseTask(w, r, true)
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
	// A lease is bound to an issue and an accountable user; neither comes from the body.
	if !task.IssueID.Valid {
		writeErrorCode(w, http.StatusUnprocessableEntity, "task_has_no_issue", "a browser lease needs a task on an issue")
		return
	}
	if !task.AccountableUserID.Valid {
		writeErrorCode(w, http.StatusForbidden, "no_accountable_user", "the task has no accountable user")
		return
	}
	res, err := h.BrowserLeases.Mint(daemonLeaseCtx(r, task), browserlease.MintRequest{
		WorkspaceID: ws, TaskID: task.ID, AgentID: task.AgentID, IssueID: task.IssueID,
		UserID: task.AccountableUserID, ProfileID: profileID, DecisionID: decisionID,
	})
	if err != nil {
		writeBrowserLeaseError(w, err)
		return
	}
	h.DecisionWorker.WakeLeaseExpiry()
	status := http.StatusCreated
	if res.Lease.State == "queued" {
		status = http.StatusAccepted
	}
	writeJSON(w, status, browserLeaseResponseOf(res))
}

func (h *Handler) RenewBrowserLease(w http.ResponseWriter, r *http.Request) {
	task, ws, ok := h.browserLeaseTask(w, r, true)
	if !ok {
		return
	}
	leaseID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "leaseId"), "lease_id")
	if !ok {
		return
	}
	res, err := h.BrowserLeases.Renew(daemonLeaseCtx(r, task), ws, task.ID, leaseID)
	if err != nil {
		writeBrowserLeaseError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, browserLeaseResponseOf(res))
}

// StatusBrowserLease — GET /api/daemon/tasks/{id}/browser-leases/{leaseId}
//
// Reads the state of the task's own lease. It never signs, so it never returns a token and it
// runs after the task ends.
func (h *Handler) StatusBrowserLease(w http.ResponseWriter, r *http.Request) {
	task, ws, ok := h.browserLeaseTask(w, r, false)
	if !ok {
		return
	}
	leaseID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "leaseId"), "lease_id")
	if !ok {
		return
	}
	res, err := h.BrowserLeases.Status(r.Context(), ws, task.ID, leaseID)
	if errors.Is(err, browserlease.ErrGateDenied) {
		err = browserlease.ErrNotFound // a missing row reads as a dropped lease
	}
	if err != nil {
		writeBrowserLeaseError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, browserLeaseResponseOf(res))
}

// SteelBrowserPluginKey is the manifest key of the plugin that turns Steel leases on
// (plugins/steel-browser, CHE-4730). Disabling or uninstalling it revokes every lease.
const SteelBrowserPluginKey = "work.cheese.steel-browser"

// browserLeaseRevoker is cheap: a few pointers, no state. It audits each revoke inside the
// transaction that ends the leases (CHE-4744).
func (h *Handler) browserLeaseRevoker() *browserlease.Revoker {
	return &browserlease.Revoker{Queries: h.Queries, Bus: h.Bus, Pool: h.TxStarter, Audit: BrowserLeaseAudit}
}

// daemonLeaseCtx attributes a lease transition to the agent whose task asked for it.
func daemonLeaseCtx(r *http.Request, task db.AgentTaskQueue) context.Context {
	return withAuditActor(r.Context(), auditActor{Type: "agent", ID: task.AgentID, TaskID: task.ID, Source: "daemon"})
}

// revokeLeasesOfDecision ends every lease bound to a decision that was withdrawn or has expired
// (CHE-4742, W3-13). The decision change has committed already, so a failure is logged, never
// returned; the lease expiry (token 15 min, hard limit 2 h) and the gateway's stream check
// (5 s) bound the exposure. An expired request (cancel_reason "expired") revokes as expired;
// every other end of the ask, or a withdrawn answer, revokes as withdrawn.
func (h *Handler) revokeLeasesOfDecision(workspaceID string, requestID pgtype.UUID, cancelReason string) {
	workspace, err := util.ParseUUID(workspaceID)
	if err != nil || !requestID.Valid {
		return
	}
	reason := browserlease.ReasonDecisionWithdrawn
	if cancelReason == "expired" {
		reason = browserlease.ReasonDecisionExpired
	}
	if _, err := h.browserLeaseRevoker().RevokeDecision(context.Background(), workspace, requestID, reason); err != nil {
		slog.Error("revoke browser leases of an ended decision failed", "workspace_id", workspaceID, "decision_id", uuidToString(requestID), "reason", reason, "error", err)
	}
}

// revokeLeasesOfExpiredDecisions revokes the leases of every decision past its expires_at,
// including an answered (approved) one, which no request-cancel path ends (CHE-4742).
// RunLeaseExpiry calls it at each deadline.
func (h *Handler) revokeLeasesOfExpiredDecisions(ctx context.Context) error {
	expired, err := h.Queries.ListExpiredDecisionBrowserLeases(ctx, decisionBatchSize)
	if err != nil {
		return err
	}
	var failures error
	revoker := h.browserLeaseRevoker()
	for _, e := range expired {
		_, err := revoker.RevokeDecision(ctx, e.WorkspaceID, e.DecisionID, browserlease.ReasonDecisionExpired)
		failures = errors.Join(failures, err)
	}
	return failures
}

// revokeLeasesOfWithdrawal covers a withdrawal whose request may be untouched: a withdrawn
// answer that reopens the ask leaves Request empty, so the answer names the decision.
func (h *Handler) revokeLeasesOfWithdrawal(workspaceID string, w decisiontx.Withdrawal) {
	id := w.Request.ID
	if !id.Valid {
		id = w.Answer.DecisionRequestID
	}
	h.revokeLeasesOfDecision(workspaceID, id, w.Request.CancelReason.String)
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
	revoked, err := h.browserLeaseRevoker().Revoke(daemonLeaseCtx(r, task), browserlease.Filter{
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
	revoked, err := h.browserLeaseRevoker().Revoke(withAuditActor(r.Context(), h.auditActorFromRequest(r, wsUUID)), browserlease.Filter{WorkspaceID: wsUUID}, browserlease.ReasonRevokeAll)
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
	if _, err := h.browserLeaseRevoker().Revoke(withAuditActor(r.Context(), h.auditActorFromRequest(r, installation.WorkspaceID)), filter, browserlease.ReasonPluginDisabled); err != nil {
		slog.Error("revoke browser leases of a disabled plugin failed", append(logger.RequestAttrs(r), "error", err, "workspace_id", uuidToString(installation.WorkspaceID))...)
	}
}
