package passkey

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	revokedSubject = "A passkey was removed from your Multica account"
	eventRevoked   = "passkey_revoked"
)

// revocationChallenge is sha256("passkey_revoke" || target credential id ||
// nonce): the signature names the one credential being removed.
func revocationChallenge(targetID pgtype.UUID, nonce []byte) []byte {
	hash := sha256.New()
	hash.Write([]byte(PurposeRevoke))
	hash.Write(targetID.Bytes[:])
	hash.Write(nonce)
	return hash.Sum(nil)
}

// revocable refuses a target that is not the user's, is already revoked, or
// would leave the user with no usable passkey.
func revocable(target db.UserPasskey, userID pgtype.UUID, usable []db.UserPasskey) error {
	if target.UserID != userID {
		return forbidden("credential_not_yours: no such passkey")
	}
	if target.RevokedAt.Valid {
		return conflict("already_revoked: this passkey is already revoked")
	}
	for _, key := range usable {
		if key.ID != target.ID {
			return nil
		}
	}
	return conflict("last_passkey: add another passkey before revoking this one")
}

// IssueRevocation opens the user's one assertion challenge for revoking the
// target credential, superseding any challenge they had open. Any of their
// usable passkeys, the target included, may answer it.
func (service *Service) IssueRevocation(ctx context.Context, userID, targetID pgtype.UUID) (Issued, error) {
	target, err := service.Queries.GetPasskey(ctx, targetID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Issued{}, forbidden("credential_not_yours: no such passkey")
	}
	if err != nil {
		return Issued{}, err
	}
	usable, err := service.Queries.ListUsablePasskeys(ctx, userID)
	if err != nil {
		return Issued{}, err
	}
	if len(usable) == 0 {
		return Issued{}, ErrNoPasskey
	}
	if err := revocable(target, userID, usable); err != nil {
		return Issued{}, err
	}
	return service.issueAssertion(ctx, userID, PurposeRevoke, targetID, usable, func(nonce []byte) []byte {
		return revocationChallenge(targetID, nonce)
	})
}

// Revoke retires the target credential once the user signed its revocation
// with a usable passkey. The signer may be the target only while another
// usable passkey remains. The notice goes out after the commit. A sign-count
// regression marks the signing credential suspect, outside the aborted
// transaction.
func (service *Service) Revoke(ctx context.Context, userID, targetID pgtype.UUID, assertion json.RawMessage, email string) error {
	challengeID, credential, err := parseEnvelope(assertion)
	if err != nil {
		return err
	}
	tx, err := service.Tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	queries := db.New(tx)
	signer, err := service.verifyAssertion(ctx, queries, verification{
		UserID: userID, Purpose: PurposeRevoke, SubjectID: targetID, ChallengeID: challengeID, Credential: credential, Also: targetID,
		Expect: func(nonce []byte) []byte { return revocationChallenge(targetID, nonce) },
	})
	if err != nil {
		tx.Rollback(ctx)
		var refusal Refusal
		if errors.As(err, &refusal) && refusal.Suspect.Valid {
			if markErr := service.MarkSuspect(ctx, refusal.Suspect); markErr != nil {
				return markErr
			}
		}
		return busyOr(err)
	}
	target, err := queries.GetPasskey(ctx, targetID)
	if errors.Is(err, pgx.ErrNoRows) {
		return forbidden("credential_not_yours: no such passkey")
	}
	if err != nil {
		return err
	}
	usable, err := queries.ListUsablePasskeys(ctx, userID)
	if err != nil {
		return err
	}
	if err := revocable(target, userID, usable); err != nil {
		return err
	}
	if err := queries.RevokePasskey(ctx, targetID); err != nil {
		return err
	}
	if err := writeEvent(ctx, queries, userID, eventRevoked, map[string]any{
		"passkey_id": uuid.UUID(targetID.Bytes).String(), "signed_by": uuid.UUID(signer.Bytes).String(),
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if service.Mail != nil {
		service.notify(email, revokedSubject, "<p>A passkey was removed from your Multica account. If this was not you, contact support now.</p>")
	}
	return nil
}
