package maintenance

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/mail"

	"github.com/multica-ai/multica/server/internal/passkey"
	"github.com/multica-ai/multica/server/internal/util"
)

// PasskeyRecovery is the C00 recovery command's answer (CHE-2798 P6, B3).
type PasskeyRecovery struct {
	UserID     string `json:"user_id"`
	State      string `json:"passkey_state"`
	Generation int    `json:"session_generation"`
	Revoked    int    `json:"revoked_passkeys"`
}

// RecoverPasskeys runs the recovery transaction for the person with this email.
func (s *Service) RecoverPasskeys(ctx context.Context, email string) (PasskeyRecovery, error) {
	if address, err := mail.ParseAddress(email); err != nil || address.Address != email {
		return PasskeyRecovery{}, ErrInvalid
	}
	recovery, err := passkey.RecoverUser(ctx, s.pool, email)
	switch {
	case errors.Is(err, passkey.ErrUserNotFound):
		return PasskeyRecovery{}, ErrNotFound
	case err != nil:
		return PasskeyRecovery{}, err
	}
	// The audit row carries the generation and key count; the log carries who.
	slog.Warn("passkey recovery", "user_id", recovery.UserID, "generation", recovery.Generation, "revoked", recovery.Revoked)
	return PasskeyRecovery{UserID: util.UUIDToString(recovery.UserID), State: passkey.StateRecoveryRequired, Generation: recovery.Generation, Revoked: recovery.Revoked}, nil
}

func (s *Service) handlePasskeyRecovery(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Email string `json:"email"`
	}
	if !readRequest(w, r, &request) {
		return
	}
	recovery, err := s.RecoverPasskeys(r.Context(), request.Email)
	if err != nil {
		respond(w, Job{}, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		Recovery PasskeyRecovery `json:"passkey_recovery"`
	}{recovery})
}
