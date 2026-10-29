package passkey

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/passkey/passkeytest"
)

// revokeFixture is a user with two keys: f.device (f.key) and second.
type revokeFixture struct {
	*fixture
	mailer    *mailbox
	second    *passkeytest.Authenticator
	secondKey pgtype.UUID
}

func newRevokeFixture(test *testing.T) *revokeFixture {
	test.Helper()
	f, mailer := newEnrollFixture(test)
	second := passkeytest.New(test)
	return &revokeFixture{fixture: f, mailer: mailer, second: second, secondKey: f.enroll(f.user, second, startCount)}
}

func (f *revokeFixture) issueRevocation(target pgtype.UUID) (Issued, string) {
	f.test.Helper()
	issued, err := f.service.IssueRevocation(context.Background(), f.user, target)
	if err != nil {
		f.test.Fatal(err)
	}
	return issued, challengeOf(f.test, issued)
}

func (f *revokeFixture) revoke(target pgtype.UUID, assertion []byte) error {
	return f.service.Revoke(context.Background(), f.user, target, assertion, testEmail)
}

func (f *revokeFixture) revoked(key pgtype.UUID) bool {
	f.test.Helper()
	return f.count(`SELECT count(*) FROM user_passkey WHERE id = $1 AND revoked_at IS NOT NULL`, key) == 1
}

func (f *revokeFixture) suspect(key pgtype.UUID) bool {
	f.test.Helper()
	return f.count(`SELECT count(*) FROM user_passkey WHERE id = $1 AND suspect_at IS NOT NULL`, key) == 1
}

func (f *revokeFixture) counter(key pgtype.UUID) int64 {
	f.test.Helper()
	var counter int64
	if err := f.pool.QueryRow(context.Background(), `SELECT sign_count FROM user_passkey WHERE id = $1`, key).Scan(&counter); err != nil {
		f.test.Fatal(err)
	}
	return counter
}

func TestRevokeByAnotherPasskeyRevokesTheTargetAndRecordsIt(test *testing.T) {
	f := newRevokeFixture(test)
	issued, challenge := f.issueRevocation(f.secondKey)
	if err := f.revoke(f.secondKey, f.signAs(f.device, issued, challenge, startCount+1, nil)); err != nil {
		test.Fatal(err)
	}
	if !f.revoked(f.secondKey) || f.revoked(f.key) {
		test.Errorf("revoked: target %v signer %v, want true false", f.revoked(f.secondKey), f.revoked(f.key))
	}
	if got := f.counter(f.key); got != startCount+1 {
		test.Errorf("signer counter = %d, want %d", got, startCount+1)
	}
	if got := f.count(`SELECT count(*) FROM passkey_event WHERE user_id = $1 AND action = 'passkey_revoked'`, f.user); got != 1 {
		test.Errorf("passkey_revoked events = %d, want 1", got)
	}
	if subjects := f.mailer.subjects(); len(subjects) != 1 || subjects[0] != revokedSubject {
		test.Errorf("mail = %v, want one %q", subjects, revokedSubject)
	}
}

func TestRevokedPasskeyCanNoLongerApprove(test *testing.T) {
	f := newRevokeFixture(test)
	pending, pendingChallenge := f.issueFor(f.user, f.action, testPayload)
	held := f.signAs(f.second, pending, pendingChallenge, startCount+1, nil)

	issued, challenge := f.issueRevocation(f.secondKey)
	if err := f.revoke(f.secondKey, f.signAs(f.device, issued, challenge, startCount+1, nil)); err != nil {
		test.Fatal(err)
	}
	_, err := f.verify(held)
	f.wantRefusal(err, http.StatusForbidden, "challenge_superseded")

	fresh, freshChallenge := f.issue()
	_, err = f.verify(f.signAs(f.second, fresh, freshChallenge, startCount+2, nil))
	f.wantRefusal(err, http.StatusForbidden, "assertion_rejected")
	if got := f.counter(f.secondKey); got != startCount {
		test.Errorf("revoked key counter = %d, want %d", got, startCount)
	}

	again, againChallenge := f.issue()
	if _, err := f.verify(f.signAs(f.device, again, againChallenge, startCount+2, nil)); err != nil {
		test.Errorf("the remaining passkey must still approve: %v", err)
	}
}

// The key being removed is left out of the allowed list, so it cannot vouch for
// its own removal: a stolen key must not be able to retire the genuine one.
func TestRevokeIsNeverSignedByTheTarget(test *testing.T) {
	f := newRevokeFixture(test)
	issued, challenge := f.issueRevocation(f.key)
	err := f.revoke(f.key, f.signAs(f.device, issued, challenge, startCount+1, nil))
	f.wantRefusal(err, http.StatusForbidden, "assertion_rejected")
	if f.revoked(f.key) || f.revoked(f.secondKey) {
		test.Error("a credential was revoked on the target's own signature")
	}
}

func TestRevokeIssuesOnlyForTheOtherKeys(test *testing.T) {
	f := newRevokeFixture(test)
	issued, challenge := f.issueRevocation(f.key)
	// The other key signs; the target's own key is not in the allowed list.
	if err := f.revoke(f.key, f.signAs(f.second, issued, challenge, startCount+1, nil)); err != nil {
		test.Fatal(err)
	}
	if !f.revoked(f.key) || f.revoked(f.secondKey) {
		test.Errorf("revoked: target %v other %v, want true false", f.revoked(f.key), f.revoked(f.secondKey))
	}
}

func TestRevokeWithoutAnotherUsableKeyPointsToTheMailedCode(test *testing.T) {
	cases := map[string]func(f *revokeFixture){
		"only key": func(f *revokeFixture) {
			f.mustExec(`UPDATE user_passkey SET revoked_at = now() WHERE id = $1`, f.secondKey)
		},
		"other key suspect": func(f *revokeFixture) {
			f.mustExec(`UPDATE user_passkey SET suspect_at = now() WHERE id = $1`, f.secondKey)
		},
	}
	for name, setup := range cases {
		test.Run(name, func(test *testing.T) {
			f := newRevokeFixture(test)
			setup(f)
			_, err := f.service.IssueRevocation(context.Background(), f.user, f.key)
			f.wantRefusal(err, http.StatusConflict, "no_other_passkey")
			if f.revoked(f.key) {
				test.Error("the passkey was revoked without a signer")
			}
		})
	}
}

// The library refuses a session whose allowed list is no longer all usable.
func TestRevokeRefusesWhenTheSignerDiesBeforeSigning(test *testing.T) {
	f := newRevokeFixture(test)
	issued, challenge := f.issueRevocation(f.key)
	f.mustExec(`UPDATE user_passkey SET suspect_at = now() WHERE id = $1`, f.secondKey)
	err := f.revoke(f.key, f.signAs(f.second, issued, challenge, startCount+1, nil))
	f.wantRefusal(err, http.StatusForbidden, "assertion_rejected")
	if f.revoked(f.key) {
		test.Error("the passkey was revoked on a dead signer")
	}
}

func TestRevokeCanRemoveASuspectPasskey(test *testing.T) {
	f := newRevokeFixture(test)
	f.mustExec(`UPDATE user_passkey SET suspect_at = now() WHERE id = $1`, f.secondKey)
	issued, challenge := f.issueRevocation(f.secondKey)
	if err := f.revoke(f.secondKey, f.signAs(f.device, issued, challenge, startCount+1, nil)); err != nil {
		test.Fatal(err)
	}
	if !f.revoked(f.secondKey) {
		test.Error("the suspect passkey was not revoked")
	}
}

func TestRevokeRefusesAnAlreadyRevokedPasskey(test *testing.T) {
	f := newRevokeFixture(test)
	f.mustExec(`UPDATE user_passkey SET revoked_at = now() WHERE id = $1`, f.secondKey)
	_, err := f.service.IssueRevocation(context.Background(), f.user, f.secondKey)
	f.wantRefusal(err, http.StatusConflict, "already_revoked")
}

func TestRevokeNeverTouchesAnotherUsersCredential(test *testing.T) {
	f := newRevokeFixture(test)
	stranger := newUser(test, f.pool)
	strangerKey := f.enroll(stranger, passkeytest.New(test), startCount)

	_, err := f.service.IssueRevocation(context.Background(), f.user, strangerKey)
	f.wantRefusal(err, http.StatusForbidden, "credential_not_yours")

	// A challenge issued for the user's own key cannot be replayed against the stranger's.
	issued, challenge := f.issueRevocation(f.secondKey)
	err = f.revoke(strangerKey, f.signAs(f.device, issued, challenge, startCount+1, nil))
	f.wantRefusal(err, http.StatusForbidden, "assertion_rejected")
	if f.revoked(strangerKey) || f.revoked(f.secondKey) {
		test.Error("a credential was revoked")
	}
}

func TestRevokeRefusesAnAssertionFromAnotherUsersKey(test *testing.T) {
	f := newRevokeFixture(test)
	stranger := passkeytest.New(test)
	f.enroll(newUser(test, f.pool), stranger, startCount)
	issued, challenge := f.issueRevocation(f.secondKey)
	err := f.revoke(f.secondKey, f.signAs(stranger, issued, challenge, startCount+1, nil))
	f.wantRefusal(err, http.StatusForbidden, "assertion_rejected")
	if f.revoked(f.secondKey) {
		test.Error("revoked on a stranger's signature")
	}
}

func TestRevokeRefusesAMissingAssertion(test *testing.T) {
	f := newRevokeFixture(test)
	f.issueRevocation(f.secondKey)
	f.wantRefusal(f.revoke(f.secondKey, nil), http.StatusForbidden, "assertion_required")
}

func TestRevokeRefusesAnApprovalChallenge(test *testing.T) {
	f := newRevokeFixture(test)
	issued, challenge := f.issue()
	err := f.revoke(f.secondKey, f.signAs(f.device, issued, challenge, startCount+1, nil))
	f.wantRefusal(err, http.StatusForbidden, "assertion_rejected")
	if f.revoked(f.secondKey) {
		test.Error("an approval signature revoked a passkey")
	}
}

func TestRevokeRefusesAReplay(test *testing.T) {
	f := newRevokeFixture(test)
	issued, challenge := f.issueRevocation(f.secondKey)
	signed := f.signAs(f.device, issued, challenge, startCount+1, nil)
	if err := f.revoke(f.secondKey, signed); err != nil {
		test.Fatal(err)
	}
	f.wantRefusal(f.revoke(f.secondKey, signed), http.StatusForbidden, "assertion_rejected")
}

func TestDelayedApprovalAfterARevocationIsSupersededWithoutSuspicion(test *testing.T) {
	f := newRevokeFixture(test)
	pending, pendingChallenge := f.issue()
	delayed := f.sign(pending, pendingChallenge, startCount+1, nil)

	issued, challenge := f.issueRevocation(f.secondKey)
	if err := f.revoke(f.secondKey, f.sign(issued, challenge, startCount+2, nil)); err != nil {
		test.Fatal(err)
	}
	_, err := f.verify(delayed)
	f.wantRefusal(err, http.StatusForbidden, "challenge_superseded")
	if count, suspect := f.stored(); count != startCount+2 || suspect {
		test.Errorf("counter %d suspect %v, want %d false", count, suspect, startCount+2)
	}
}

func TestDelayedRevocationAfterAnApprovalIsSupersededWithoutSuspicion(test *testing.T) {
	f := newRevokeFixture(test)
	issued, challenge := f.issueRevocation(f.secondKey)
	delayed := f.sign(issued, challenge, startCount+1, nil)

	pending, pendingChallenge := f.issue()
	if _, err := f.verify(f.sign(pending, pendingChallenge, startCount+2, nil)); err != nil {
		test.Fatal(err)
	}
	f.wantRefusal(f.revoke(f.secondKey, delayed), http.StatusForbidden, "challenge_superseded")
	if count, suspect := f.stored(); count != startCount+2 || suspect {
		test.Errorf("counter %d suspect %v, want %d false", count, suspect, startCount+2)
	}
	if f.revoked(f.secondKey) {
		test.Error("a superseded revocation took effect")
	}
}

func TestClonedAuthenticatorReplayOnAnOpenRevocationIsRefusedAndMarked(test *testing.T) {
	f := newRevokeFixture(test)
	issued, challenge := f.issueRevocation(f.secondKey)
	err := f.revoke(f.secondKey, f.sign(issued, challenge, startCount-1, nil))
	f.wantRefusal(err, http.StatusForbidden, "assertion_rejected")
	if !f.suspect(f.key) {
		test.Error("the cloned credential was not marked suspect")
	}
	if f.revoked(f.secondKey) {
		test.Error("the target was revoked on a regressed counter")
	}
	if got := f.counter(f.key); got != startCount {
		test.Errorf("counter = %d, want it unchanged at %d", got, startCount)
	}
}

func TestRevocationNoticeFailureDoesNotUndoTheRevocation(test *testing.T) {
	f := newRevokeFixture(test)
	f.mailer.err = errors.New("provider down")
	issued, challenge := f.issueRevocation(f.secondKey)
	if err := f.revoke(f.secondKey, f.signAs(f.device, issued, challenge, startCount+1, nil)); err != nil {
		test.Fatal(err)
	}
	if !f.revoked(f.secondKey) {
		test.Error("the revocation was undone by a failed notice")
	}
}

func (f *revokeFixture) mustExec(query string, args ...any) {
	f.test.Helper()
	if _, err := f.pool.Exec(context.Background(), query, args...); err != nil {
		f.test.Fatal(err)
	}
}
