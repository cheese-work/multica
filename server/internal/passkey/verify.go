package passkey

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var (
	errAssertionRequired   = forbidden("assertion_required: sign this approval with your passkey")
	errAssertionRejected   = forbidden("assertion_rejected: the passkey assertion was not accepted")
	errChallengeSuperseded = forbidden("challenge_superseded: a newer passkey request replaced this one; touch your passkey again")
	errChallengeExpired    = forbidden("challenge_expired: the passkey request expired; touch your passkey again")
)

// assertionEnvelope is what the client posts as the approval's assertion: the
// challenge it answers and the WebAuthn credential response.
type assertionEnvelope struct {
	ChallengeID string          `json:"challenge_id"`
	Credential  json.RawMessage `json:"credential"`
}

// VerifyApproval proves the user signed this action's payload. Call it inside
// the approval transaction after the payload hash matched; it locks the
// challenge and the credential (NOWAIT) and returns the credential used.
func (service *Service) VerifyApproval(ctx context.Context, queries *db.Queries, userID, actionID pgtype.UUID, payloadSHA256 string, assertion json.RawMessage) (pgtype.UUID, error) {
	challengeID, credential, err := parseEnvelope(assertion)
	if err != nil {
		return pgtype.UUID{}, err
	}
	return service.verifyAssertion(ctx, queries, verification{
		UserID: userID, Purpose: PurposeDecisionApprove, SubjectID: actionID, ChallengeID: challengeID, Credential: credential,
		Expect: func(nonce []byte) []byte { return approvalChallenge(actionID, payloadSHA256, nonce) },
	})
}

func parseEnvelope(assertion json.RawMessage) (pgtype.UUID, json.RawMessage, error) {
	var envelope assertionEnvelope
	if len(assertion) == 0 || json.Unmarshal(assertion, &envelope) != nil || len(envelope.Credential) == 0 {
		return pgtype.UUID{}, nil, errAssertionRequired
	}
	challengeID, err := util.ParseUUID(envelope.ChallengeID)
	if err != nil {
		return pgtype.UUID{}, nil, errAssertionRejected
	}
	return challengeID, envelope.Credential, nil
}

type verification struct {
	UserID      pgtype.UUID
	Purpose     Purpose
	SubjectID   pgtype.UUID
	ChallengeID pgtype.UUID
	Credential  json.RawMessage
	// Also names a second credential the verifier must lock together with the
	// signing one, in ascending id order.
	Also pgtype.UUID
	// Expect rebuilds the challenge bytes the server must have issued.
	Expect func(nonce []byte) []byte
}

// verifyAssertion is the one place an assertion is verified. Order: lock the
// challenge; it must be open, the user's, and of this purpose and subject;
// lock the signing credential; then the library validation; then the counter.
// Every consumer of an assertion goes through it.
func (service *Service) verifyAssertion(ctx context.Context, queries *db.Queries, request verification) (pgtype.UUID, error) {
	challenge, err := queries.LockPasskeyChallengeNowait(ctx, request.ChallengeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, errAssertionRejected
	}
	if err != nil {
		return pgtype.UUID{}, err
	}
	if challenge.UserID != request.UserID || challenge.Purpose != string(request.Purpose) || challenge.SubjectID != request.SubjectID || challenge.ConsumedAt.Valid {
		return pgtype.UUID{}, errAssertionRejected
	}
	if challenge.SupersededAt.Valid {
		return pgtype.UUID{}, errChallengeSuperseded
	}
	if challenge.Expired {
		return pgtype.UUID{}, errChallengeExpired
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(request.Credential)
	if err != nil {
		return pgtype.UUID{}, errAssertionRejected
	}
	key, err := lockSigningPasskey(ctx, queries, parsed.RawID, request.Also)
	if err != nil {
		return pgtype.UUID{}, err
	}
	if key.UserID != request.UserID || key.RevokedAt.Valid || key.SuspectAt.Valid {
		return pgtype.UUID{}, errAssertionRejected
	}
	var session webauthn.SessionData
	if err := json.Unmarshal(challenge.SessionData, &session); err != nil {
		return pgtype.UUID{}, errAssertionRejected
	}
	if !bytes.Equal(request.Expect(challenge.Nonce), decodeChallenge(session.Challenge)) {
		return pgtype.UUID{}, errAssertionRejected
	}
	usable, err := queries.ListUsablePasskeys(ctx, request.UserID)
	if err != nil {
		return pgtype.UUID{}, err
	}
	if _, err := service.WebAuthn.ValidateLogin(newWebAuthnUser(request.UserID, usable), session, parsed); err != nil {
		return pgtype.UUID{}, errAssertionRejected
	}
	counter := int64(parsed.Response.AuthenticatorData.Counter)
	if (counter != 0 || key.SignCount != 0) && counter <= key.SignCount {
		refusal := errAssertionRejected
		refusal.Suspect = key.ID
		return pgtype.UUID{}, refusal
	}
	if err := queries.AdvancePasskey(ctx, db.AdvancePasskeyParams{ID: key.ID, SignCount: counter}); err != nil {
		return pgtype.UUID{}, err
	}
	if err := queries.ConsumePasskeyChallenge(ctx, challenge.ID); err != nil {
		return pgtype.UUID{}, err
	}
	return key.ID, nil
}

func lockSigningPasskey(ctx context.Context, queries *db.Queries, credentialID []byte, also pgtype.UUID) (db.UserPasskey, error) {
	if !also.Valid {
		key, err := queries.LockPasskeyByCredentialNowait(ctx, credentialID)
		if errors.Is(err, pgx.ErrNoRows) {
			return db.UserPasskey{}, errAssertionRejected
		}
		return key, err
	}
	signingID, err := queries.GetPasskeyIDByCredential(ctx, credentialID)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.UserPasskey{}, errAssertionRejected
	}
	if err != nil {
		return db.UserPasskey{}, err
	}
	locked, err := queries.LockPasskeysNowait(ctx, []pgtype.UUID{signingID, also})
	if err != nil {
		return db.UserPasskey{}, err
	}
	for _, key := range locked {
		if key.ID == signingID {
			return key, nil
		}
	}
	return db.UserPasskey{}, errAssertionRejected
}

func decodeChallenge(encoded string) []byte {
	var decoded protocol.URLEncodedBase64
	if err := decoded.UnmarshalJSON([]byte(`"` + encoded + `"`)); err != nil {
		return nil
	}
	return decoded
}
