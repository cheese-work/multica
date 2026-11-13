package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// DecisionReadScope is the PAT scope that opens the state read of one action
// decision, GET /api/decisions/{id}/state. It opens no other route.
const DecisionReadScope = "decisions:read"

// DecisionStateResponse is what became of one action decision. approver_id is
// set once the owner approved; revoked_at and withdrawn_at are set when the
// approval was taken back or the request was withdrawn.
type DecisionStateResponse struct {
	DecisionID    string          `json:"decision_id"`
	ActionID      string          `json:"action_id"`
	ActionType    string          `json:"action_type"`
	ActionVersion int32           `json:"action_version"`
	State         string          `json:"state"`
	ApproverID    *string         `json:"approver_id"`
	AnsweredAt    *string         `json:"answered_at"`
	WithdrawnAt   *string         `json:"withdrawn_at"`
	RevokedAt     *string         `json:"revoked_at"`
	ExpiresAt     *string         `json:"expires_at"`
	Params        json.RawMessage `json:"params"`
}

// decisionStateOf names the one state a reader acts on. Precedence: a revoke
// outranks everything, then a withdrawal, a rejection, expiry, approval.
func decisionStateOf(request db.DecisionRequest, action db.DecisionAction, now time.Time) string {
	switch {
	case action.RevokedAt.Valid:
		return "revoked"
	case request.WithdrawnAt.Valid:
		return "withdrawn"
	}
	switch action.State {
	case "rejected":
		return "rejected"
	case "expired":
		return "expired"
	case "cancelled":
		return "cancelled"
	case "executed":
		return "approved"
	case "failed":
		return "failed"
	}
	if action.ExpiresAt.Valid && !action.ExpiresAt.Time.After(now) {
		return "expired"
	}
	return "pending"
}

// GetDecisionState reads one action decision by its request id. Only a
// decisions:read PAT of the workspace owner reaches it.
func (h *Handler) GetDecisionState(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := h.requireScopedPATOwner(w, r, DecisionReadScope)
	if !ok {
		return
	}
	requestID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "decision id")
	if !ok {
		return
	}
	workspace := parseUUID(workspaceID)
	request, err := h.Queries.GetDecisionRequest(r.Context(), db.GetDecisionRequestParams{WorkspaceID: workspace, ID: requestID})
	if err != nil {
		writeDecisionStateError(w, err)
		return
	}
	action, err := h.Queries.GetDecisionActionByRequest(r.Context(), db.GetDecisionActionByRequestParams{DecisionRequestID: request.ID, WorkspaceID: workspace})
	if err != nil {
		writeDecisionStateError(w, err)
		return
	}
	answeredAt := timestampToPtr(action.ApprovedAt)
	answer, err := h.Queries.GetLatestDecisionAnswer(r.Context(), db.GetLatestDecisionAnswerParams{WorkspaceID: workspace, ID: request.ID})
	switch {
	case err == nil:
		answeredAt = timestampToPtr(answer.CreatedAt)
	case !errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusInternalServerError, "failed to read decision state")
		return
	}
	writeJSON(w, http.StatusOK, DecisionStateResponse{
		DecisionID: uuidToString(request.ID), ActionID: uuidToString(action.ID),
		ActionType: action.ActionType, ActionVersion: action.ActionVersion,
		State:      decisionStateOf(request, action, time.Now()),
		ApproverID: uuidToPtr(action.ApprovedByUserID), AnsweredAt: answeredAt,
		WithdrawnAt: timestampToPtr(request.WithdrawnAt), RevokedAt: timestampToPtr(action.RevokedAt),
		ExpiresAt: timestampToPtr(action.ExpiresAt), Params: json.RawMessage(action.Params),
	})
}

func writeDecisionStateError(w http.ResponseWriter, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "decision not found")
		return
	}
	writeError(w, http.StatusInternalServerError, "failed to read decision state")
}
