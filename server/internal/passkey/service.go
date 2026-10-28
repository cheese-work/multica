package passkey

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type Service struct {
	WebAuthn *webauthn.WebAuthn
	Queries  *db.Queries
	Tx       Beginner
	Mail     Mailer
	CodeKey  []byte // HMAC key for enrollment codes (MULTICA_PASSKEY_ENROLL_KEY)
}

type Issued struct {
	ChallengeID pgtype.UUID
	Options     json.RawMessage
	ExpiresAt   time.Time
}

// Refusal is a verification refusal the caller shows as an HTTP status.
// Suspect names a credential the caller must mark after it rolls back: the
// verifier runs inside the approval transaction, which a refusal aborts.
type Refusal struct {
	Status  int
	Message string
	Suspect pgtype.UUID
}

func (refusal Refusal) Error() string { return refusal.Message }

var ErrNoPasskey = errors.New("passkey: none enrolled")

func forbidden(message string) Refusal {
	return Refusal{Status: http.StatusForbidden, Message: message}
}

// approvalChallenge is sha256(action_id || payload_sha256 || nonce): the
// signature covers the exact payload the human saw.
func approvalChallenge(actionID pgtype.UUID, payloadSHA256 string, nonce []byte) []byte {
	hash := sha256.New()
	hash.Write(actionID.Bytes[:])
	hash.Write([]byte(payloadSHA256))
	hash.Write(nonce)
	return hash.Sum(nil)
}

// IssueApproval opens the user's one assertion challenge for approving this
// action payload, superseding any challenge they had open.
func (service *Service) IssueApproval(ctx context.Context, userID, actionID pgtype.UUID, payloadSHA256 string) (Issued, error) {
	rows, err := service.Queries.ListUsablePasskeys(ctx, userID)
	if err != nil {
		return Issued{}, err
	}
	return service.issueAssertion(ctx, userID, PurposeDecisionApprove, actionID, rows, func(nonce []byte) []byte {
		return approvalChallenge(actionID, payloadSHA256, nonce)
	})
}

// issueAssertion opens the user's one open challenge, answerable by any of
// their usable passkeys, over the bytes expect derives from the fresh nonce.
func (service *Service) issueAssertion(ctx context.Context, userID pgtype.UUID, purpose Purpose, subjectID pgtype.UUID, rows []db.UserPasskey, expect func(nonce []byte) []byte) (Issued, error) {
	if len(rows) == 0 {
		return Issued{}, ErrNoPasskey
	}
	user := newWebAuthnUser(userID, rows)
	allowed := make([]protocol.CredentialDescriptor, len(rows))
	for i, row := range rows {
		allowed[i] = protocol.CredentialDescriptor{Type: protocol.PublicKeyCredentialType, CredentialID: row.CredentialID}
	}
	var options []byte
	challenge, err := IssueChallenge(ctx, service.Tx, userID, purpose, subjectID, func(nonce []byte) ([]byte, error) {
		assertion, session, err := service.WebAuthn.BeginLogin(user,
			webauthn.WithChallenge(expect(nonce)),
			webauthn.WithUserVerification(protocol.VerificationRequired),
			webauthn.WithAllowedCredentials(allowed))
		if err != nil {
			return nil, err
		}
		if options, err = json.Marshal(assertion); err != nil {
			return nil, err
		}
		return json.Marshal(session)
	})
	if err != nil {
		return Issued{}, err
	}
	return Issued{ChallengeID: challenge.ID, Options: options, ExpiresAt: challenge.ExpiresAt}, nil
}

// MarkSuspect flags a credential after a sign-count regression. A suspect
// credential is no longer offered or accepted.
func (service *Service) MarkSuspect(ctx context.Context, credentialID pgtype.UUID) error {
	return service.Queries.MarkPasskeySuspect(ctx, credentialID)
}
