package handler

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/logger"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// Management of inbound webhook endpoints (docs/engineering/workflow-webhook-guard.md,
// "Management"). Owner and admin only. The secret is returned once, on create
// and rotate; it is stored sealed and never logged or listed.

const (
	workflowWebhookSecretPrefix  = "whs_"
	workflowWebhookSecretBytes   = 32
	workflowWebhookPreviousGrace = 24 * time.Hour
	workflowWebhookDefaultRate   = 60

	workflowWebhookActivityCreated  = "workflow_webhook_created"
	workflowWebhookActivityRotated  = "workflow_webhook_rotated"
	workflowWebhookActivityDisabled = "workflow_webhook_disabled"
)

var workflowWebhookNameRE = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

type WorkflowWebhookEndpointResponse struct {
	ID                string  `json:"id"`
	Name              string  `json:"name"`
	RatePerMin        int32   `json:"rate_per_min"`
	CreatedAt         string  `json:"created_at"`
	DisabledAt        *string `json:"disabled_at"`
	PreviousExpiresAt *string `json:"previous_expires_at"`
	// Secret is set only on the create and rotate responses.
	Secret string `json:"secret,omitempty"`
}

type CreateWorkflowWebhookRequest struct {
	Name       string `json:"name"`
	RatePerMin *int32 `json:"rate_per_min"`
}

func workflowWebhookToResponse(ep db.WorkflowWebhookEndpoint, secret string) WorkflowWebhookEndpointResponse {
	resp := WorkflowWebhookEndpointResponse{
		ID:         uuidToString(ep.ID),
		Name:       ep.Name,
		RatePerMin: ep.RatePerMin,
		CreatedAt:  timestampToString(ep.CreatedAt),
		DisabledAt: timestampToPtr(ep.DisabledAt),
		Secret:     secret,
	}
	if len(ep.PreviousSecretSealed) > 0 {
		resp.PreviousExpiresAt = timestampToPtr(ep.PreviousExpiresAt)
	}
	return resp
}

func newWorkflowWebhookSecret() (string, error) {
	raw := make([]byte, workflowWebhookSecretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return workflowWebhookSecretPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// authorizeWebhookAdmin resolves the workflow and checks that the caller may
// manage it. It writes the error response itself.
func (h *Handler) authorizeWebhookAdmin(w http.ResponseWriter, r *http.Request) (db.Member, db.IssueWorkflow, bool) {
	workspaceID := h.resolveWorkspaceID(r)
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return db.Member{}, db.IssueWorkflow{}, false
	}
	member, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found")
	if !ok {
		return db.Member{}, db.IssueWorkflow{}, false
	}
	idUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workflow id")
	if !ok {
		return db.Member{}, db.IssueWorkflow{}, false
	}
	workflow, err := h.Queries.GetIssueWorkflow(r.Context(), db.GetIssueWorkflowParams{ID: idUUID, WorkspaceID: wsUUID})
	if err != nil {
		writeError(w, http.StatusNotFound, "workflow not found")
		return db.Member{}, db.IssueWorkflow{}, false
	}
	if !canEditIssueWorkflow(member, workflow) {
		writeError(w, http.StatusForbidden, "only the workflow owner and workspace admins can manage its webhooks")
		return db.Member{}, db.IssueWorkflow{}, false
	}
	return member, workflow, true
}

func (h *Handler) ListWorkflowWebhooks(w http.ResponseWriter, r *http.Request) {
	_, workflow, ok := h.authorizeWebhookAdmin(w, r)
	if !ok {
		return
	}
	rows, err := h.Queries.ListWorkflowWebhookEndpoints(r.Context(), db.ListWorkflowWebhookEndpointsParams{
		WorkspaceID: workflow.WorkspaceID, WorkflowID: workflow.ID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list webhooks")
		return
	}
	out := make([]WorkflowWebhookEndpointResponse, len(rows))
	for i, ep := range rows {
		out[i] = workflowWebhookToResponse(ep, "")
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) CreateWorkflowWebhook(w http.ResponseWriter, r *http.Request) {
	member, workflow, ok := h.authorizeWebhookAdmin(w, r)
	if !ok {
		return
	}
	if h.VCSSecretBox == nil {
		writeError(w, http.StatusServiceUnavailable, "webhook secrets are not configured on this server")
		return
	}
	var req CreateWorkflowWebhookRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !workflowWebhookNameRE.MatchString(req.Name) {
		writeError(w, http.StatusBadRequest, "name must be 1-32 characters of a-z, 0-9, _ or -")
		return
	}
	rate := int32(workflowWebhookDefaultRate)
	if req.RatePerMin != nil {
		rate = *req.RatePerMin
	}
	if rate < 1 || rate > 6000 {
		writeError(w, http.StatusBadRequest, "rate_per_min must be between 1 and 6000")
		return
	}
	secret, err := newWorkflowWebhookSecret()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create webhook")
		return
	}
	sealed, err := h.VCSSecretBox.Seal([]byte(secret))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create webhook")
		return
	}
	ep, ok := h.writeWebhookChange(w, r, member, workflow, workflowWebhookActivityCreated, "failed to create webhook",
		func(q *db.Queries) (db.WorkflowWebhookEndpoint, error) {
			return q.CreateWorkflowWebhookEndpoint(r.Context(), db.CreateWorkflowWebhookEndpointParams{
				WorkspaceID: workflow.WorkspaceID, WorkflowID: workflow.ID, Name: req.Name,
				SecretSealed: sealed, RatePerMin: rate, CreatedBy: member.UserID,
			})
		})
	if !ok {
		return
	}
	writeJSON(w, http.StatusCreated, workflowWebhookToResponse(ep, secret))
}

func (h *Handler) RotateWorkflowWebhook(w http.ResponseWriter, r *http.Request) {
	member, workflow, ok := h.authorizeWebhookAdmin(w, r)
	if !ok {
		return
	}
	if h.VCSSecretBox == nil {
		writeError(w, http.StatusServiceUnavailable, "webhook secrets are not configured on this server")
		return
	}
	endpointID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "endpointId"), "endpoint id")
	if !ok {
		return
	}
	secret, err := newWorkflowWebhookSecret()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to rotate webhook")
		return
	}
	sealed, err := h.VCSSecretBox.Seal([]byte(secret))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to rotate webhook")
		return
	}
	ep, ok := h.writeWebhookChange(w, r, member, workflow, workflowWebhookActivityRotated, "failed to rotate webhook",
		func(q *db.Queries) (db.WorkflowWebhookEndpoint, error) {
			if err := h.requireEndpointOfWorkflow(r.Context(), q, workflow, endpointID); err != nil {
				return db.WorkflowWebhookEndpoint{}, err
			}
			return q.RotateWorkflowWebhookEndpointSecret(r.Context(), db.RotateWorkflowWebhookEndpointSecretParams{
				ID: endpointID, SecretSealed: sealed,
				PreviousExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(workflowWebhookPreviousGrace), Valid: true},
			})
		})
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, workflowWebhookToResponse(ep, secret))
}

func (h *Handler) DisableWorkflowWebhook(w http.ResponseWriter, r *http.Request) {
	member, workflow, ok := h.authorizeWebhookAdmin(w, r)
	if !ok {
		return
	}
	endpointID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "endpointId"), "endpoint id")
	if !ok {
		return
	}
	ep, ok := h.writeWebhookChange(w, r, member, workflow, workflowWebhookActivityDisabled, "failed to disable webhook",
		func(q *db.Queries) (db.WorkflowWebhookEndpoint, error) {
			if err := h.requireEndpointOfWorkflow(r.Context(), q, workflow, endpointID); err != nil {
				return db.WorkflowWebhookEndpoint{}, err
			}
			return q.DisableWorkflowWebhookEndpoint(r.Context(), endpointID)
		})
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, workflowWebhookToResponse(ep, ""))
}

// requireEndpointOfWorkflow returns pgx.ErrNoRows when the endpoint belongs to
// another workflow, so it looks the same as an unknown id.
func (h *Handler) requireEndpointOfWorkflow(ctx context.Context, q *db.Queries, workflow db.IssueWorkflow, endpointID pgtype.UUID) error {
	ep, err := q.GetWorkflowWebhookEndpoint(ctx, endpointID)
	if err != nil {
		return err
	}
	if ep.WorkflowID != workflow.ID {
		return pgx.ErrNoRows
	}
	return nil
}

// writeWebhookChange runs change and writes its one activity row in a single
// transaction, so a change never lands without its audit row. It writes the
// error response itself. The activity details name the endpoint, never a secret.
func (h *Handler) writeWebhookChange(
	w http.ResponseWriter, r *http.Request, member db.Member, workflow db.IssueWorkflow,
	action, failMsg string, change func(q *db.Queries) (db.WorkflowWebhookEndpoint, error),
) (db.WorkflowWebhookEndpoint, bool) {
	ctx := r.Context()
	fail := func(err error) (db.WorkflowWebhookEndpoint, bool) {
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			writeError(w, http.StatusNotFound, "webhook endpoint not found")
		case isUniqueViolation(err):
			writeError(w, http.StatusConflict, "a webhook endpoint with this name already exists on the workflow")
		default:
			slog.Error("workflow webhook change failed", append(logger.RequestAttrs(r), "error", err, "action", action)...)
			writeError(w, http.StatusInternalServerError, failMsg)
		}
		return db.WorkflowWebhookEndpoint{}, false
	}
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return fail(err)
	}
	defer tx.Rollback(ctx)
	qtx := h.Queries.WithTx(tx)
	ep, err := change(qtx)
	if err != nil {
		return fail(err)
	}
	details, _ := json.Marshal(map[string]string{
		"workflow_id": uuidToString(workflow.ID),
		"endpoint_id": uuidToString(ep.ID),
		"name":        ep.Name,
	})
	if _, err := qtx.CreateActivity(ctx, db.CreateActivityParams{
		ID:          dbid.NewV7(),
		WorkspaceID: workflow.WorkspaceID,
		ActorType:   pgtype.Text{String: "member", Valid: true},
		ActorID:     member.UserID,
		Action:      action,
		Details:     details,
	}); err != nil {
		return fail(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fail(err)
	}
	return ep, true
}
