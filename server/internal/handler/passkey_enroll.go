package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/passkey"
	"github.com/multica-ai/multica/server/internal/util"
)

const enrollBodyLimit = 64 << 10

// interactiveCaller admits only an interactive browser session: a CLI token,
// PAT or machine actor never changes a human's passkeys.
func interactiveCaller(writer http.ResponseWriter, request *http.Request) (pgtype.UUID, bool) {
	var userID pgtype.UUID
	if request.Header.Get("X-Actor-Source") != "interactive_jwt" {
		writeError(writer, http.StatusForbidden, errNotTheHuman.message)
		return userID, false
	}
	userID, err := util.ParseUUID(request.Header.Get("X-User-ID"))
	if err != nil {
		writeError(writer, http.StatusForbidden, errNotTheHuman.message)
		return userID, false
	}
	return userID, true
}

// enrollCaller gates both enrollment endpoints. The flag stays closed until
// the mailbox-isolation decision is recorded.
func (handler *Handler) enrollCaller(writer http.ResponseWriter, request *http.Request) (pgtype.UUID, bool) {
	userID, ok := interactiveCaller(writer, request)
	if !ok {
		return userID, false
	}
	if !featureflags.PasskeyEnrollmentEnabled(request.Context(), handler.FeatureFlags) {
		writeFeatureDisabled(writer, "passkey_enrollment_disabled", "passkey enrollment is not open")
		return userID, false
	}
	if handler.Passkeys == nil {
		writeFeatureDisabled(writer, "passkeys_unavailable", "passkeys are not configured")
		return userID, false
	}
	return userID, true
}

func writeEnrollError(writer http.ResponseWriter, err error) {
	var refusal passkey.Refusal
	switch {
	case errors.As(err, &refusal):
		writeError(writer, refusal.Status, refusal.Message)
	case errors.Is(err, passkey.ErrDeliveryUnavailable):
		writeError(writer, http.StatusServiceUnavailable, "stepup_delivery_unavailable: the enrollment code cannot be mailed right now")
	case errors.Is(err, passkey.ErrBusy):
		writer.Header().Set("Retry-After", "1")
		writeError(writer, errBusyRetry.status, errBusyRetry.message)
	default:
		writeError(writer, http.StatusInternalServerError, "failed to enroll the passkey")
	}
}

type enrollStartResponse struct {
	CeremonyID string          `json:"ceremony_id"`
	Options    json.RawMessage `json:"options"`
	ExpiresAt  time.Time       `json:"expires_at"`
}

// StartPasskeyEnrollment mails the signed-in human a code and returns the
// WebAuthn creation options bound to it.
func (handler *Handler) StartPasskeyEnrollment(writer http.ResponseWriter, request *http.Request) {
	userID, ok := handler.enrollCaller(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), decisionActionDeadline)
	defer cancel()
	user, err := handler.Queries.GetUser(ctx, userID)
	if err != nil {
		writeEnrollError(writer, err)
		return
	}
	started, err := handler.Passkeys.StartEnrollment(ctx, userID, user.Email)
	if err != nil {
		writeEnrollError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, enrollStartResponse{CeremonyID: uuidToString(started.CeremonyID), Options: started.Options, ExpiresAt: started.ExpiresAt})
}

type enrollFinishRequest struct {
	CeremonyID  string          `json:"ceremony_id"`
	Code        string          `json:"code"`
	Attestation json.RawMessage `json:"attestation"`
}

// FinishPasskeyEnrollment stores the credential when the mailed code and the
// attestation both hold for the caller's ceremony.
func (handler *Handler) FinishPasskeyEnrollment(writer http.ResponseWriter, request *http.Request) {
	userID, ok := handler.enrollCaller(writer, request)
	if !ok {
		return
	}
	var body enrollFinishRequest
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, enrollBodyLimit)).Decode(&body); err != nil || len(body.Attestation) == 0 {
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
		writeEnrollError(writer, err)
		return
	}
	if err := handler.Passkeys.FinishEnrollment(ctx, userID, ceremonyID, body.Code, body.Attestation, user.Email); err != nil {
		writeEnrollError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]bool{"enrolled": true})
}
