package passkey

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Mailer sends the enrollment code and the account notices. SendPrivileged
// returns ErrDeliveryUnavailable unless a real provider took the message: the
// stdout fallback of login codes must never carry an enrollment code.
type Mailer interface {
	SendPrivileged(to, subject, htmlBody string) error
}

var ErrDeliveryUnavailable = errors.New("passkey: no real mail provider")

const (
	maxCodeAttempts     = 5
	maxFailedCeremonies = 3
	minCodeKeyLen       = 32

	codeSubject     = "Your Multica passkey enrollment code"
	enrolledSubject = "A passkey was added to your Multica account"
	lockedSubject   = "Multica passkey enrollment locked"
)

const (
	eventEnrolled = "passkey_enrolled"
	eventLocked   = "passkey_enroll_locked"
)

type Started struct {
	CeremonyID pgtype.UUID
	Options    json.RawMessage
	ExpiresAt  time.Time
}

func (service *Service) deliverable() bool {
	return service.Mail != nil && len(service.CodeKey) >= minCodeKeyLen
}

func codeMAC(key []byte, userID pgtype.UUID, code string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(userID.Bytes[:])
	mac.Write([]byte(code))
	return mac.Sum(nil)
}

func newCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n), nil
}

func isUniqueViolation(err error) bool {
	var pgError *pgconn.PgError
	return errors.As(err, &pgError) && pgError.Code == "23505"
}

// notify sends an account notice. The state change it reports is committed, so
// a failed notice is logged, not returned.
func (service *Service) notify(email, subject, body string) {
	if err := service.Mail.SendPrivileged(email, subject, body); err != nil {
		slog.Warn("passkey notice not delivered", "subject", subject, "error", err)
	}
}

// StartEnrollment opens the user's one enrollment ceremony and mails its
// code. Without a real provider nothing is stored: the ceremony commits only
// after the provider took the mail.
func (service *Service) StartEnrollment(ctx context.Context, userID pgtype.UUID, email string) (Started, error) {
	if !service.deliverable() {
		return Started{}, ErrDeliveryUnavailable
	}
	rows, err := service.Queries.ListUsablePasskeys(ctx, userID)
	if err != nil {
		return Started{}, err
	}
	excluded := make([]protocol.CredentialDescriptor, len(rows))
	for i, row := range rows {
		excluded[i] = protocol.CredentialDescriptor{Type: protocol.PublicKeyCredentialType, CredentialID: row.CredentialID}
	}
	creation, session, err := service.WebAuthn.BeginRegistration(newWebAuthnUser(userID, rows),
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			UserVerification: protocol.VerificationRequired, ResidentKey: protocol.ResidentKeyRequirementPreferred,
		}),
		webauthn.WithExclusions(excluded),
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation))
	if err != nil {
		return Started{}, err
	}
	options, err := json.Marshal(creation)
	if err != nil {
		return Started{}, err
	}
	sessionData, err := json.Marshal(session)
	if err != nil {
		return Started{}, err
	}
	row, err := service.mailCeremony(ctx, userID, email, ceremony{
		Purpose: ceremonyEnroll, Session: sessionData, Subject: codeSubject, Lead: "Your passkey enrollment code is",
	})
	if err != nil {
		return Started{}, err
	}
	return Started{CeremonyID: row.ID, Options: options, ExpiresAt: row.ExpiresAt.Time}, nil
}

const (
	ceremonyEnroll = "enroll"
	ceremonyRevoke = "revoke"
)

// ceremony is what mailCeremony stores and mails: a code the user must type
// back, optionally bound to one passkey.
type ceremony struct {
	Purpose string
	Target  pgtype.UUID
	Session []byte
	Subject string
	Lead    string
}

// mailCeremony opens the user's one code ceremony, superseding any open one,
// and mails its code. The ceremony commits only after the provider took the
// mail.
func (service *Service) mailCeremony(ctx context.Context, userID pgtype.UUID, email string, kind ceremony) (db.InsertPasskeyEnrollCeremonyRow, error) {
	code, err := newCode()
	if err != nil {
		return db.InsertPasskeyEnrollCeremonyRow{}, err
	}
	tx, err := service.Tx.Begin(ctx)
	if err != nil {
		return db.InsertPasskeyEnrollCeremonyRow{}, err
	}
	defer tx.Rollback(ctx)
	queries := db.New(tx)
	if err := lockEnrollment(ctx, queries, userID, true); err != nil {
		return db.InsertPasskeyEnrollCeremonyRow{}, err
	}
	if err := queries.ConsumeOpenPasskeyEnrollCeremony(ctx, userID); err != nil {
		return db.InsertPasskeyEnrollCeremonyRow{}, err
	}
	row, err := queries.InsertPasskeyEnrollCeremony(ctx, db.InsertPasskeyEnrollCeremonyParams{
		UserID: userID, CodeHmac: codeMAC(service.CodeKey, userID, code), SessionData: kind.Session,
		Purpose: kind.Purpose, TargetPasskeyID: kind.Target,
	})
	if err != nil {
		return row, busyOr(err)
	}
	if err := queries.CountPasskeyEnrollStart(ctx, userID); err != nil {
		return row, err
	}
	body := fmt.Sprintf("<p>%s</p><p><b>%s</b></p><p>It expires in 10 minutes. If you did not ask for it, ignore this email.</p>", kind.Lead, code)
	if err := service.Mail.SendPrivileged(email, kind.Subject, body); err != nil {
		if errors.Is(err, ErrDeliveryUnavailable) {
			return row, err
		}
		return row, fmt.Errorf("%w: %w", ErrDeliveryUnavailable, err)
	}
	return row, tx.Commit(ctx)
}

// lockEnrollment takes the user's limit row, the first lock of every
// enrollment transaction, and refuses a locked user. A start is also refused
// past 3 an hour or 10 a day.
func lockEnrollment(ctx context.Context, queries *db.Queries, userID pgtype.UUID, starting bool) error {
	if err := queries.EnsurePasskeyEnrollLimit(ctx, userID); err != nil {
		return err
	}
	limit, err := queries.LockPasskeyEnrollLimitNowait(ctx, userID)
	if err != nil {
		return busyOr(err)
	}
	switch {
	case limit.Locked:
		return forbidden("enrollment_locked: too many failed enrollments; try again in 24 hours")
	case starting && (limit.HourCount >= 3 || limit.DayCount >= 10):
		return Refusal{Status: http.StatusTooManyRequests, Message: "enroll_rate_limited: too many enrollment attempts; try again later"}
	}
	return nil
}

func conflict(message string) Refusal { return Refusal{Status: http.StatusConflict, Message: message} }

// FinishEnrollment checks the mailed code and the attestation together and, if
// both hold, stores the credential for the ceremony's user. A wrong code or a
// rejected attestation spends one of the ceremony's five attempts, and that
// spend commits even though the call fails.
func (service *Service) FinishEnrollment(ctx context.Context, userID, ceremonyID pgtype.UUID, code string, attestation []byte, email string) error {
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
		return forbidden("ceremony_not_yours: no such enrollment")
	case err != nil:
		return busyOr(err)
	case ceremony.UserID != userID:
		return forbidden("ceremony_not_yours: no such enrollment")
	case ceremony.Purpose != ceremonyEnroll:
		return forbidden("ceremony_not_yours: no such enrollment")
	case ceremony.ConsumedAt.Valid:
		return conflict("ceremony_used: start a new enrollment")
	case ceremony.Expired:
		return conflict("ceremony_expired: start a new enrollment")
	}

	reject := func(refusal Refusal) error {
		return service.spendAttempt(ctx, tx, queries, ceremony.ID, userID, email, refusal)
	}

	if !hmac.Equal(ceremony.CodeHmac, codeMAC(service.CodeKey, userID, code)) {
		return reject(forbidden("invalid_code: the code does not match"))
	}
	var session webauthn.SessionData
	if err := json.Unmarshal(ceremony.SessionData, &session); err != nil {
		return err
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(attestation)
	if err != nil {
		return reject(forbidden("attestation_rejected: the passkey response is malformed"))
	}
	credential, err := service.WebAuthn.CreateCredential(newWebAuthnUser(userID, nil), session, parsed)
	if err != nil {
		return reject(forbidden("attestation_rejected: the passkey response was refused"))
	}

	transports := make([]string, len(credential.Transport))
	for i, transport := range credential.Transport {
		transports[i] = string(transport)
	}
	stored, err := queries.InsertPasskey(ctx, db.InsertPasskeyParams{
		UserID: userID, CredentialID: credential.ID, PublicKey: credential.PublicKey, AttestationType: credential.AttestationType,
		Aaguid: credential.Authenticator.AAGUID, SignCount: int64(credential.Authenticator.SignCount), Transports: transports,
		Flags: int16(credential.Flags.ProtocolValue()),
	})
	if isUniqueViolation(err) {
		return conflict("credential_exists: this passkey is already enrolled")
	} else if err != nil {
		return err
	}
	if err := queries.ConsumePasskeyEnrollCeremony(ctx, ceremony.ID); err != nil {
		return err
	}
	if err := writeEvent(ctx, queries, userID, eventEnrolled, map[string]any{"passkey_id": uuid.UUID(stored.ID.Bytes).String()}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	service.notify(email, enrolledSubject, "<p>A passkey was added to your Multica account. If this was not you, contact support now.</p>")
	return nil
}

// spendAttempt records one failed try on the ceremony and commits that spend
// even though the call fails. The fifth failed try may lock enrollment.
func (service *Service) spendAttempt(ctx context.Context, tx pgx.Tx, queries *db.Queries, ceremonyID, userID pgtype.UUID, email string, refusal Refusal) error {
	attempts, err := queries.BumpPasskeyEnrollAttempt(ctx, ceremonyID)
	if err != nil {
		return err
	}
	var after func()
	if attempts >= maxCodeAttempts {
		if after, err = service.lockIfRepeated(ctx, queries, userID, email); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if after != nil {
		after()
	}
	return refusal
}

// lockIfRepeated locks enrollment for 24 hours once the third ceremony in a
// day has failed, and returns the notice to send after the commit.
func (service *Service) lockIfRepeated(ctx context.Context, queries *db.Queries, userID pgtype.UUID, email string) (func(), error) {
	failed, err := queries.CountFailedPasskeyEnrollCeremonies(ctx, userID)
	if err != nil || failed < maxFailedCeremonies {
		return nil, err
	}
	if err := queries.LockPasskeyEnrollment(ctx, userID); err != nil {
		return nil, err
	}
	if err := writeEvent(ctx, queries, userID, eventLocked, map[string]any{"failed_ceremonies": failed}); err != nil {
		return nil, err
	}
	return func() {
		service.notify(email, lockedSubject, "<p>Passkey enrollment on your Multica account is locked for 24 hours after repeated failed attempts. If this was not you, contact support now.</p>")
	}, nil
}

func writeEvent(ctx context.Context, queries *db.Queries, userID pgtype.UUID, action string, details map[string]any) error {
	encoded, err := json.Marshal(details)
	if err != nil {
		return err
	}
	return queries.InsertPasskeyEvent(ctx, db.InsertPasskeyEventParams{UserID: userID, Action: action, Details: encoded})
}
