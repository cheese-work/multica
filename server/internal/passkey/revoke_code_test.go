package passkey

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/passkey/passkeytest"
)

func (f *revokeFixture) startByCode(target pgtype.UUID) Started {
	f.test.Helper()
	started, err := f.service.StartRevocationByCode(context.Background(), f.user, target, testEmail)
	if err != nil {
		f.test.Fatal(err)
	}
	return started
}

func (f *revokeFixture) revokeByCode(target pgtype.UUID, started Started, code string) error {
	return f.service.RevokeByCode(context.Background(), f.user, target, started.CeremonyID, code, testEmail)
}

func (f *revokeFixture) attempts(started Started) int {
	f.test.Helper()
	return f.count(`SELECT attempts FROM passkey_enroll_ceremony WHERE id = $1`, started.CeremonyID)
}

func TestMailedCodeRevokesTheLastUsablePasskey(test *testing.T) {
	f := newRevokeFixture(test)
	f.mustExec(`UPDATE user_passkey SET revoked_at = now() WHERE id = $1`, f.secondKey)
	started := f.startByCode(f.key)
	if err := f.revokeByCode(f.key, started, f.mailer.lastCode(test)); err != nil {
		test.Fatal(err)
	}
	if !f.revoked(f.key) {
		test.Error("the passkey was not revoked")
	}
	if got := f.count(`SELECT count(*) FROM passkey_event WHERE user_id = $1 AND action = 'passkey_revoked'`, f.user); got != 1 {
		test.Errorf("passkey_revoked events = %d, want 1", got)
	}
	if subjects := f.mailer.subjects(); len(subjects) != 2 || subjects[0] != revokeCodeSubject || subjects[1] != revokedSubject {
		test.Errorf("mail = %v, want the code then a removal notice", subjects)
	}
	f.wantRefusal(f.revokeByCode(f.key, started, "000000"), http.StatusConflict, "ceremony_used")
}

func TestMailedCodeWrongCodeSpendsAnAttemptAndRevokesNothing(test *testing.T) {
	f := newRevokeFixture(test)
	started := f.startByCode(f.key)
	f.wantRefusal(f.revokeByCode(f.key, started, "000000"), http.StatusForbidden, "invalid_code")
	if f.revoked(f.key) || f.attempts(started) != 1 {
		test.Errorf("revoked %v attempts %d, want false 1", f.revoked(f.key), f.attempts(started))
	}
}

func TestMailedCodeIsDeadAfterFiveWrongCodes(test *testing.T) {
	f := newRevokeFixture(test)
	started := f.startByCode(f.key)
	code := f.mailer.lastCode(test)
	for range 5 {
		f.wantRefusal(f.revokeByCode(f.key, started, "000000"), http.StatusForbidden, "invalid_code")
	}
	f.wantRefusal(f.revokeByCode(f.key, started, code), http.StatusConflict, "ceremony_used")
	if f.revoked(f.key) {
		test.Error("a burned ceremony still revoked")
	}
}

func TestMailedCodeIsBoundToUserPurposeAndTarget(test *testing.T) {
	f := newRevokeFixture(test)
	started := f.startByCode(f.key)
	code := f.mailer.lastCode(test)

	// Another target.
	f.wantRefusal(f.revokeByCode(f.secondKey, started, code), http.StatusForbidden, "ceremony_not_yours")
	// Another user.
	stranger := newUser(test, f.pool)
	err := f.service.RevokeByCode(context.Background(), stranger, f.key, started.CeremonyID, code, testEmail)
	f.wantRefusal(err, http.StatusForbidden, "ceremony_not_yours")
	if f.revoked(f.key) || f.revoked(f.secondKey) {
		test.Fatal("a mismatched ceremony revoked a passkey")
	}
}

func TestRevocationCeremonyCannotFinishAnEnrollmentAndViceVersa(test *testing.T) {
	f := newRevokeFixture(test)
	revocation := f.startByCode(f.key)
	code := f.mailer.lastCode(test)
	attestation := startedCeremony{Started: revocation}
	err := f.service.FinishEnrollment(context.Background(), f.user, revocation.CeremonyID, code, f.attest(f.device, attestation, nil), testEmail)
	f.wantRefusal(err, http.StatusForbidden, "ceremony_not_yours")

	enrolment := f.start()
	f.wantRefusal(f.revokeByCode(f.key, enrolment.Started, f.mailer.lastCode(test)), http.StatusForbidden, "ceremony_not_yours")
	if f.revoked(f.key) {
		test.Error("an enrollment ceremony revoked a passkey")
	}
}

func TestMailedCodeRefusesSomeoneElsesAndRevokedPasskeys(test *testing.T) {
	f := newRevokeFixture(test)
	stranger := newUser(test, f.pool)
	strangerKey := f.enroll(stranger, passkeytest.New(test), startCount)
	_, err := f.service.StartRevocationByCode(context.Background(), f.user, strangerKey, testEmail)
	f.wantRefusal(err, http.StatusForbidden, "credential_not_yours")

	f.mustExec(`UPDATE user_passkey SET revoked_at = now() WHERE id = $1`, f.secondKey)
	_, err = f.service.StartRevocationByCode(context.Background(), f.user, f.secondKey, testEmail)
	f.wantRefusal(err, http.StatusConflict, "already_revoked")
}

func TestMailedCodeExpires(test *testing.T) {
	f := newRevokeFixture(test)
	started := f.startByCode(f.key)
	f.mustExec(`UPDATE passkey_enroll_ceremony SET expires_at = now() - interval '1 second' WHERE id = $1`, started.CeremonyID)
	f.wantRefusal(f.revokeByCode(f.key, started, f.mailer.lastCode(test)), http.StatusConflict, "ceremony_expired")
}

func TestMailedCodeRevocationFailsClosedWithoutRealDelivery(test *testing.T) {
	f := newRevokeFixture(test)
	f.service.Mail = nil
	if _, err := f.service.StartRevocationByCode(context.Background(), f.user, f.key, testEmail); !errors.Is(err, ErrDeliveryUnavailable) {
		test.Fatalf("err = %v, want ErrDeliveryUnavailable", err)
	}
	if n := f.count(`SELECT count(*) FROM passkey_enroll_ceremony WHERE user_id = $1`, f.user); n != 0 {
		test.Fatalf("%d ceremonies left behind", n)
	}
}
