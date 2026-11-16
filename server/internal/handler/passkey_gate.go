package handler

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/passkey"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type gateChangeRequest struct {
	State     string          `json:"state"`
	Assertion json.RawMessage `json:"assertion"`
}

// gateCaller admits only the workspace owner on an interactive session. The
// gate decides who must sign decisions, so no machine actor, PAT or admin
// moves it.
func (handler *Handler) gateCaller(writer http.ResponseWriter, request *http.Request) (userID, workspaceID pgtype.UUID, ok bool) {
	if userID, ok = interactiveCaller(writer, request); !ok {
		return
	}
	if handler.Passkeys == nil {
		writeFeatureDisabled(writer, "passkeys_unavailable", "passkeys are not configured")
		return userID, workspaceID, false
	}
	id := workspaceIDFromURL(request, "id")
	if _, ok = handler.requireWorkspaceRole(writer, request, id, "workspace not found", "owner"); !ok {
		return
	}
	workspaceID, ok = parseUUIDOrBadRequest(writer, id, "workspace id")
	return
}

func readGateChange(writer http.ResponseWriter, request *http.Request) (gateChangeRequest, bool) {
	var body gateChangeRequest
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, enrollBodyLimit)).Decode(&body); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid request body")
		return body, false
	}
	if body.State != passkey.GateOff && body.State != passkey.GateBlock {
		writeError(writer, http.StatusBadRequest, "invalid_state: state must be off or block")
		return body, false
	}
	return body, true
}

// ChallengeDecisionGate opens the owner's assertion challenge for moving the
// workspace's decision gate to the requested state.
func (handler *Handler) ChallengeDecisionGate(writer http.ResponseWriter, request *http.Request) {
	userID, workspaceID, ok := handler.gateCaller(writer, request)
	if !ok {
		return
	}
	body, ok := readGateChange(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), decisionActionDeadline)
	defer cancel()
	issued, err := handler.Passkeys.IssueGateChange(ctx, userID, workspaceID, body.State)
	if err != nil {
		writeRevokeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, challengeResponse{
		ChallengeID: uuidToString(issued.ChallengeID), Options: issued.Options, ExpiresAt: issued.ExpiresAt,
	})
}

// SetDecisionGate moves the decision gate once the owner signed the change
// with a usable passkey. Turning the gate off needs the same assertion.
func (handler *Handler) SetDecisionGate(writer http.ResponseWriter, request *http.Request) {
	userID, workspaceID, ok := handler.gateCaller(writer, request)
	if !ok {
		return
	}
	body, ok := readGateChange(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), decisionActionDeadline)
	defer cancel()
	audit := func(ctx context.Context, queries *db.Queries, previous string, changed db.SetWorkspaceDecisionGateRow) error {
		if err := handler.writeAudit(ctx, queries, request, auditInput{
			Kind: "workspace.decision_gate.update", WorkspaceID: workspaceID, TargetID: workspaceID,
			Before: map[string]any{"decision_passkey_gate": previous},
			After:  map[string]any{"decision_passkey_gate": changed.DecisionPasskeyGate, "policy_revision": changed.PolicyRevision},
		}); err != nil {
			return auditWriteError{err}
		}
		return nil
	}
	if err := handler.Passkeys.SetDecisionGate(ctx, userID, workspaceID, body.State, body.Assertion, audit); err != nil {
		if failedAudit(writer, request, err, "workspace.decision_gate.update") {
			return
		}
		writeRevokeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"decision_passkey_gate": body.State})
}
