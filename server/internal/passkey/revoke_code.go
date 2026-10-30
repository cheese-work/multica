package passkey

import (
	"context"
	"crypto/hmac"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const revokeCodeSubject = "Your Multica passkey removal code"

// StartRevocationByCode mails the user a code that retires one of their
// passkeys without a signature. It is the way out when no other passkey can
// sign, so it may remove the last usable one. It shares the enrollment
// ceremony limits and lockout.
func (service *Service) StartRevocationByCode(ctx context.Context, userID, targetID pgtype.UUID, email string) (Started, error) {
	if !service.deliverable() {
		return Started{}, ErrDeliveryUnavailable
	}
	target, err := service.Queries.GetPasskey(ctx, targetID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Started{}, forbidden("credential_not_yours: no such passkey")
	}
	if err != nil {
		return Started{}, err
	}
	if err := revocable(target, userID); err != nil {
		return Started{}, err
	}
	row, err := service.mailCeremony(ctx, userID, email, ceremony{
		Purpose: ceremonyRevoke, Target: targetID, Session: []byte("{}"),
		Subject: revokeCodeSubject, Lead: "Your code to remove a passkey is",
	})
	if err != nil {
		return Started{}, err
	}
	return Started{CeremonyID: row.ID, ExpiresAt: row.ExpiresAt.Time}, nil
}

// RevokeByCode retires the target once the mailed code of the user's
// revocation ceremony for that target is typed back. A wrong code spends one of
// the ceremony's five attempts, and that spend commits even though the call
// fails.
func (service *Service) RevokeByCode(ctx context.Context, userID, targetID, ceremonyID pgtype.UUID, code, email string) error {
	if !service.deliverable() {
		return ErrDeliveryUnavailable
	}
	tx, err := service.Tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	queries := db.New(tx)
	if err := lockEnrollment(ctx, queries, userID, false); err != nil {
		return err
	}
	ceremony, err := queries.LockPasskeyEnrollCeremonyNowait(ctx, ceremonyID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return forbidden("ceremony_not_yours: no such revocation")
	case err != nil:
		return busyOr(err)
	case ceremony.UserID != userID, ceremony.Purpose != ceremonyRevoke, ceremony.TargetPasskeyID != targetID:
		return forbidden("ceremony_not_yours: no such revocation")
	case ceremony.ConsumedAt.Valid:
		return conflict("ceremony_used: start a new revocation")
	case ceremony.Expired:
		return conflict("ceremony_expired: start a new revocation")
	}
	if !hmac.Equal(ceremony.CodeHmac, codeMAC(service.CodeKey, userID, code)) {
		return service.spendAttempt(ctx, tx, queries, ceremony.ID, userID, email, forbidden("invalid_code: the code does not match"))
	}
	locked, err := queries.LockPasskeysNowait(ctx, []pgtype.UUID{targetID})
	if err != nil {
		return busyOr(err)
	}
	if len(locked) != 1 {
		return forbidden("credential_not_yours: no such passkey")
	}
	if err := revocable(locked[0], userID); err != nil {
		return err
	}
	if err := queries.RevokePasskey(ctx, targetID); err != nil {
		return err
	}
	if err := queries.ConsumePasskeyEnrollCeremony(ctx, ceremony.ID); err != nil {
		return err
	}
	if err := writeEvent(ctx, queries, userID, eventRevoked, map[string]any{
		"passkey_id": uuid.UUID(targetID.Bytes).String(), "via": "mailed_code",
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	service.notify(email, revokedSubject, "<p>A passkey was removed from your Multica account. If this was not you, contact support now.</p>")
	return nil
}
