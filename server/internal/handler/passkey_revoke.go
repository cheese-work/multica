package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/passkey"
)

// revokeCaller gates both revocation endpoints. Revocation only removes
// privilege, so it stays open while enrollment is closed: a lost or suspect
// passkey must be removable at any time.
func (handler *Handler) revokeCaller(writer http.ResponseWriter, request *http.Request) (userID, targetID pgtype.UUID, ok bool) {
	if userID, ok = interactiveCaller(writer, request); !ok {
		return
	}
	if handler.Passkeys == nil {
		writeFeatureDisabled(writer, "passkeys_unavailable", "passkeys are not configured")
		return userID, targetID, false
	}
	targetID, ok = parseUUIDOrBadRequest(writer, chi.URLParam(request, "id"), "id")
	return
}

func writeRevokeError(writer http.ResponseWriter, err error) {
	var refusal passkey.Refusal
	switch {
	case errors.As(err, &refusal):
		writeError(writer, refusal.Status, refusal.Message)
	case errors.Is(err, passkey.ErrNoPasskey):
		writeError(writer, http.StatusConflict, "no_passkey: no usable passkey to sign with")
	case errors.Is(err, passkey.ErrDeliveryUnavailable):
		writeError(writer, http.StatusServiceUnavailable, "stepup_delivery_unavailable: the removal code cannot be mailed right now")
	case errors.Is(err, passkey.ErrBusy):
		writer.Header().Set("Retry-After", "1")
		writeError(writer, errBusyRetry.status, errBusyRetry.message)
	default:
		writeError(writer, http.StatusInternalServerError, "failed to revoke the passkey")
	}
}

// ChallengePasskeyRevocation opens the human's assertion challenge for
// revoking one of their passkeys.
func (handler *Handler) ChallengePasskeyRevocation(writer http.ResponseWriter, request *http.Request) {
	userID, targetID, ok := handler.revokeCaller(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), decisionActionDeadline)
	defer cancel()
	issued, err := handler.Passkeys.IssueRevocation(ctx, userID, targetID)
	if err != nil {
		writeRevokeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, challengeResponse{
		ChallengeID: uuidToString(issued.ChallengeID), Options: issued.Options, ExpiresAt: issued.ExpiresAt,
	})
}

type revokeRequest struct {
	Assertion json.RawMessage `json:"assertion"`
}

// RevokePasskey revokes the passkey once the human signed the challenge with a
// usable passkey. The service marks a cloned authenticator suspect itself.
func (handler *Handler) RevokePasskey(writer http.ResponseWriter, request *http.Request) {
	userID, targetID, ok := handler.revokeCaller(writer, request)
	if !ok {
		return
	}
	var body revokeRequest
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, enrollBodyLimit)).Decode(&body); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid request body")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), decisionActionDeadline)
	defer cancel()
	user, err := handler.Queries.GetUser(ctx, userID)
	if err != nil {
		writeRevokeError(writer, err)
		return
	}
	if err := handler.Passkeys.Revoke(ctx, userID, targetID, body.Assertion, user.Email); err != nil {
		writeRevokeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]bool{"revoked": true})
}

type revokeCodeStartResponse struct {
	CeremonyID string    `json:"ceremony_id"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// StartPasskeyRevocationByCode mails the human a code that retires the passkey
// when no other passkey can sign. Like the other revocation endpoints it stays
// open while enrollment is closed.
func (handler *Handler) StartPasskeyRevocationByCode(writer http.ResponseWriter, request *http.Request) {
	userID, targetID, ok := handler.revokeCaller(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), decisionActionDeadline)
	defer cancel()
	user, err := handler.Queries.GetUser(ctx, userID)
	if err != nil {
		writeRevokeError(writer, err)
		return
	}
	started, err := handler.Passkeys.StartRevocationByCode(ctx, userID, targetID, user.Email)
	if err != nil {
		writeRevokeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, revokeCodeStartResponse{CeremonyID: uuidToString(started.CeremonyID), ExpiresAt: started.ExpiresAt})
}

type revokeCodeConfirmRequest struct {
	CeremonyID string `json:"ceremony_id"`
	Code       string `json:"code"`
}

// ConfirmPasskeyRevocationByCode revokes the passkey once the mailed code is
// typed back.
func (handler *Handler) ConfirmPasskeyRevocationByCode(writer http.ResponseWriter, request *http.Request) {
	userID, targetID, ok := handler.revokeCaller(writer, request)
	if !ok {
		return
	}
	var body revokeCodeConfirmRequest
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, enrollBodyLimit)).Decode(&body); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid request body")
		return
	}
	ceremonyID, ok := parseUUIDOrBadRequest(writer, body.CeremonyID, "ceremony_id")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), decisionActionDeadline)
	defer cancel()
	user, err := handler.Queries.GetUser(ctx, userID)
	if err != nil {
		writeRevokeError(writer, err)
		return
	}
	if err := handler.Passkeys.RevokeByCode(ctx, userID, targetID, ceremonyID, body.Code, user.Email); err != nil {
		writeRevokeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]bool{"revoked": true})
}
