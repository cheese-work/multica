package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/browserlease"
	"github.com/multica-ai/multica/server/internal/middleware"
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
		exp := res.TokenExpires
		out.TokenExpires = &exp
	}
	return out
}

func writeBrowserLeaseError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, browserlease.ErrGateDenied):
		writeErrorCode(w, http.StatusForbidden, "browser_permission_denied", err.Error())
	case errors.Is(err, browserlease.ErrDecisionRequired):
		writeErrorCode(w, http.StatusConflict, "decision_required", err.Error())
	case errors.Is(err, browserlease.ErrDecisionInvalid):
		writeErrorCode(w, http.StatusForbidden, "decision_invalid", err.Error())
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
	task, wsID, ok := h.requireDaemonTaskAccessWithWorkspace(w, r, chi.URLParam(r, "taskId"))
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
	res, err := h.BrowserLeases.Mint(r.Context(), browserlease.MintRequest{
		WorkspaceID: ws, TaskID: task.ID, AgentID: task.AgentID, IssueID: task.IssueID,
		UserID: task.AccountableUserID, ProfileID: profileID, DecisionID: decisionID,
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

func (h *Handler) RenewBrowserLease(w http.ResponseWriter, r *http.Request) {
	task, ws, ok := h.browserLeaseTask(w, r, true)
	if !ok {
		return
	}
	leaseID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "leaseId"), "lease_id")
	if !ok {
		return
	}
	res, err := h.BrowserLeases.Renew(r.Context(), ws, task.ID, leaseID)
	if err != nil {
		writeBrowserLeaseError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, browserLeaseResponseOf(res))
}

func (h *Handler) ReleaseBrowserLease(w http.ResponseWriter, r *http.Request) {
	task, ws, ok := h.browserLeaseTask(w, r, false)
	if !ok {
		return
	}
	leaseID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "leaseId"), "lease_id")
	if !ok {
		return
	}
	if err := h.BrowserLeases.Release(r.Context(), ws, task.ID, leaseID); err != nil {
		writeBrowserLeaseError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
