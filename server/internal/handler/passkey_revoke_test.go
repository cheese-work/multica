package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/passkey/passkeytest"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// twoPasskeys gives the action's human a second usable passkey next to the one
// enrollPasskey adds.
type twoPasskeys struct {
	pendingAction
	first, second     *passkeytest.Authenticator
	firstID, secondID string
	handler           *Handler
	mailer            *codeMailbox
}

func newTwoPasskeys(test *testing.T) twoPasskeys {
	test.Helper()
	action := newPendingAction(test)
	first, firstID := action.enrollPasskey(test)
	second := passkeytest.New(test)
	var secondID string
	dbfx.QueryRow(test, `INSERT INTO user_passkey (user_id, credential_id, public_key, sign_count, flags) VALUES ($1, $2, $3, $4, 5) RETURNING id`,
		action.humanID, second.CredentialID, second.COSEPublicKey(test), passkeyStart).Scan(&secondID)
	dbfx.Cleanup(test, `DELETE FROM user_passkey WHERE id = $1`, secondID)
	dbfx.Cleanup(test, `DELETE FROM passkey_event WHERE user_id = $1`, action.humanID)
	handler := passkeyHandler(test)
	mailer := &codeMailbox{}
	handler.Passkeys.Mail, handler.Passkeys.CodeKey = mailer, []byte("0123456789abcdef0123456789abcdef")
	return twoPasskeys{pendingAction: action, first: first, second: second, firstID: firstID, secondID: secondID, handler: handler, mailer: mailer}
}

// strangerPasskey is a usable passkey that belongs to another user.
func strangerPasskey(test *testing.T) string {
	test.Helper()
	user := enrollUser(test)
	device := passkeytest.New(test)
	var id string
	dbfx.QueryRow(test, `INSERT INTO user_passkey (user_id, credential_id, public_key, sign_count, flags) VALUES ($1, $2, $3, $4, 5) RETURNING id`,
		user, device.CredentialID, device.COSEPublicKey(test), passkeyStart).Scan(&id)
	return id
}

func (p twoPasskeys) request(path, id, user, source string, body any) *http.Request {
	request := withURLParam(enrollRequest("/api/passkeys/"+id+path, user, source, body), "id", id)
	return request
}

func (p twoPasskeys) challengeFor(test *testing.T, target string, status int) string {
	test.Helper()
	return testutil.Call(test, p.handler.ChallengePasskeyRevocation, p.request("/revoke/challenge", target, p.humanID, "interactive_jwt", nil)).Want(status).Text()
}

func (p twoPasskeys) issueRevocation(test *testing.T, target string) issuedChallenge {
	test.Helper()
	var issued issuedChallenge
	decodeJSON(test, p.challengeFor(test, target, http.StatusOK), &issued)
	return issued
}

func (p twoPasskeys) revoke(test *testing.T, target string, assertion any, status int) string {
	test.Helper()
	request := p.request("/revoke", target, p.humanID, "interactive_jwt", map[string]any{"assertion": assertion})
	return testutil.Call(test, p.handler.RevokePasskey, request).Want(status).Text()
}

func (p twoPasskeys) sign(test *testing.T, device *passkeytest.Authenticator, issued issuedChallenge, counter uint32) map[string]any {
	test.Helper()
	return p.assertion(test, device, issued, counter)
}

func (p twoPasskeys) revoked(test *testing.T, id string) bool {
	test.Helper()
	var revoked bool
	dbfx.QueryRow(test, `SELECT revoked_at IS NOT NULL FROM user_passkey WHERE id = $1`, id).Scan(&revoked)
	return revoked
}

func (p twoPasskeys) stored(test *testing.T, id string) (count int64, suspect bool) {
	test.Helper()
	dbfx.QueryRow(test, `SELECT sign_count, suspect_at IS NOT NULL FROM user_passkey WHERE id = $1`, id).Scan(&count, &suspect)
	return
}

func TestPasskeyRevocationNeedsAnInteractiveSession(test *testing.T) {
	p := newTwoPasskeys(test)
	for _, source := range []string{"cli_jwt", "pat", "task_token", "cloud_pat", ""} {
		testutil.Call(test, p.handler.ChallengePasskeyRevocation, p.request("/revoke/challenge", p.secondID, p.humanID, source, nil)).Want(http.StatusForbidden)
		testutil.Call(test, p.handler.RevokePasskey, p.request("/revoke", p.secondID, p.humanID, source, map[string]any{"assertion": map[string]any{}})).Want(http.StatusForbidden)
	}
}

func TestPasskeyRevocationWithoutPasskeysConfiguredIsUnavailable(test *testing.T) {
	p := newTwoPasskeys(test)
	body := testutil.Call(test, testHandler.ChallengePasskeyRevocation, p.request("/revoke/challenge", p.secondID, p.humanID, "interactive_jwt", nil)).Want(http.StatusForbidden).Text()
	if !strings.Contains(body, "passkeys_unavailable") {
		test.Fatalf("body = %q", body)
	}
}

func TestPasskeyRevocationByAnotherPasskey(test *testing.T) {
	p := newTwoPasskeys(test)
	issued := p.issueRevocation(test, p.secondID)
	p.revoke(test, p.secondID, p.sign(test, p.first, issued, passkeyStart+1), http.StatusOK)
	if !p.revoked(test, p.secondID) || p.revoked(test, p.firstID) {
		test.Errorf("revoked: target %v signer %v, want true false", p.revoked(test, p.secondID), p.revoked(test, p.firstID))
	}
	if len(p.mailer.body) != 1 {
		test.Errorf("notices = %d, want 1", len(p.mailer.body))
	}
}

func TestRevokedPasskeyCanNoLongerApprove(test *testing.T) {
	p := newTwoPasskeys(test)
	issued := p.issueRevocation(test, p.secondID)
	p.revoke(test, p.secondID, p.sign(test, p.first, issued, passkeyStart+1), http.StatusOK)

	approval := p.issue(test, p.handler)
	body := p.approveWith(test, p.handler, p.sign(test, p.second, approval, passkeyStart+1), http.StatusForbidden)
	if !strings.Contains(body, "assertion_rejected") {
		test.Fatalf("body = %q", body)
	}
	if count, _ := p.stored(test, p.secondID); count != passkeyStart {
		test.Errorf("revoked key counter = %d, want %d", count, passkeyStart)
	}
}

func TestPasskeyRevocationRefusals(test *testing.T) {
	p := newTwoPasskeys(test)
	stranger := strangerPasskey(test)

	if body := p.challengeFor(test, stranger, http.StatusForbidden); !strings.Contains(body, "credential_not_yours") {
		test.Errorf("another user's credential: %q", body)
	}
	if body := p.challengeFor(test, uuid.NewString(), http.StatusForbidden); !strings.Contains(body, "credential_not_yours") {
		test.Errorf("unknown credential: %q", body)
	}
	testutil.Call(test, p.handler.ChallengePasskeyRevocation, p.request("/revoke/challenge", "not-a-uuid", p.humanID, "interactive_jwt", nil)).Want(http.StatusBadRequest)

	dbfx.Exec(test, `UPDATE user_passkey SET revoked_at = now() WHERE id = $1`, p.secondID)
	if body := p.challengeFor(test, p.secondID, http.StatusConflict); !strings.Contains(body, "already_revoked") {
		test.Errorf("already revoked: %q", body)
	}
	if body := p.challengeFor(test, p.firstID, http.StatusConflict); !strings.Contains(body, "no_other_passkey") {
		test.Errorf("no other passkey: %q", body)
	}
	if p.revoked(test, p.firstID) || p.revoked(test, stranger) {
		test.Error("a credential was revoked")
	}
}

func TestPasskeyRevocationNeverRevokesAnotherUsersCredential(test *testing.T) {
	p := newTwoPasskeys(test)
	stranger := strangerPasskey(test)
	issued := p.issueRevocation(test, p.secondID)
	body := p.revoke(test, stranger, p.sign(test, p.first, issued, passkeyStart+1), http.StatusForbidden)
	if !strings.Contains(body, "assertion_rejected") {
		test.Fatalf("body = %q", body)
	}
	if p.revoked(test, stranger) || p.revoked(test, p.secondID) {
		test.Error("a credential was revoked")
	}
}

func TestDelayedApprovalAfterARevocationIsSuperseded(test *testing.T) {
	p := newTwoPasskeys(test)
	pending := p.issue(test, p.handler)
	delayed := p.sign(test, p.first, pending, passkeyStart+1)

	issued := p.issueRevocation(test, p.secondID)
	p.revoke(test, p.secondID, p.sign(test, p.first, issued, passkeyStart+2), http.StatusOK)

	body := p.approveWith(test, p.handler, delayed, http.StatusForbidden)
	if !strings.Contains(body, "challenge_superseded") {
		test.Fatalf("body = %q", body)
	}
	if count, suspect := p.stored(test, p.firstID); count != passkeyStart+2 || suspect {
		test.Errorf("counter %d suspect %v, want %d false", count, suspect, passkeyStart+2)
	}
}

func TestDelayedRevocationAfterAnApprovalIsSuperseded(test *testing.T) {
	p := newTwoPasskeys(test)
	issued := p.issueRevocation(test, p.secondID)
	delayed := p.sign(test, p.first, issued, passkeyStart+1)

	approval := p.issue(test, p.handler)
	p.approveWith(test, p.handler, p.sign(test, p.first, approval, passkeyStart+2), http.StatusOK)

	body := p.revoke(test, p.secondID, delayed, http.StatusForbidden)
	if !strings.Contains(body, "challenge_superseded") {
		test.Fatalf("body = %q", body)
	}
	if count, suspect := p.stored(test, p.firstID); count != passkeyStart+2 || suspect {
		test.Errorf("counter %d suspect %v, want %d false", count, suspect, passkeyStart+2)
	}
	if p.revoked(test, p.secondID) {
		test.Error("a superseded revocation took effect")
	}
}

func TestClonedAuthenticatorOnAnOpenRevocationIsRefusedAndMarked(test *testing.T) {
	p := newTwoPasskeys(test)
	issued := p.issueRevocation(test, p.secondID)
	body := p.revoke(test, p.secondID, p.sign(test, p.first, issued, passkeyStart-1), http.StatusForbidden)
	if !strings.Contains(body, "assertion_rejected") {
		test.Fatalf("body = %q", body)
	}
	if _, suspect := p.stored(test, p.firstID); !suspect {
		test.Error("the cloned credential was not marked suspect")
	}
	if p.revoked(test, p.secondID) {
		test.Error("the target was revoked on a regressed counter")
	}
}

func (p twoPasskeys) startCode(test *testing.T, target string, status int) string {
	test.Helper()
	return testutil.Call(test, p.handler.StartPasskeyRevocationByCode, p.request("/revoke/code", target, p.humanID, "interactive_jwt", nil)).Want(status).Text()
}

func (p twoPasskeys) confirmCode(test *testing.T, target, ceremony, code string, status int) string {
	test.Helper()
	request := p.request("/revoke/confirm", target, p.humanID, "interactive_jwt", map[string]any{"ceremony_id": ceremony, "code": code})
	return testutil.Call(test, p.handler.ConfirmPasskeyRevocationByCode, request).Want(status).Text()
}

func TestPasskeyRevocationByMailedCodeRemovesTheLastUsablePasskey(test *testing.T) {
	p := newTwoPasskeys(test)
	dbfx.Exec(test, `UPDATE user_passkey SET revoked_at = now() WHERE id = $1`, p.secondID)
	p.challengeFor(test, p.firstID, http.StatusConflict)

	var started struct {
		CeremonyID string `json:"ceremony_id"`
	}
	decodeJSON(test, p.startCode(test, p.firstID, http.StatusOK), &started)
	p.confirmCode(test, p.firstID, started.CeremonyID, "000000", http.StatusForbidden)
	if p.revoked(test, p.firstID) {
		test.Fatal("revoked on a wrong code")
	}
	p.confirmCode(test, p.firstID, started.CeremonyID, p.mailer.code(test), http.StatusOK)
	if !p.revoked(test, p.firstID) {
		test.Error("the passkey was not revoked")
	}
}

func TestPasskeyRevocationByMailedCodeRefusals(test *testing.T) {
	p := newTwoPasskeys(test)
	for _, source := range []string{"cli_jwt", "pat", "task_token", "cloud_pat", ""} {
		testutil.Call(test, p.handler.StartPasskeyRevocationByCode, p.request("/revoke/code", p.secondID, p.humanID, source, nil)).Want(http.StatusForbidden)
		testutil.Call(test, p.handler.ConfirmPasskeyRevocationByCode, p.request("/revoke/confirm", p.secondID, p.humanID, source, map[string]any{})).Want(http.StatusForbidden)
	}
	if body := p.startCode(test, strangerPasskey(test), http.StatusForbidden); !strings.Contains(body, "credential_not_yours") {
		test.Errorf("another user's credential: %q", body)
	}
	p.confirmCode(test, p.secondID, "not-a-uuid", "000000", http.StatusBadRequest)

	p.handler.Passkeys.Mail = nil
	if body := p.startCode(test, p.secondID, http.StatusServiceUnavailable); !strings.Contains(body, "stepup_delivery_unavailable") {
		test.Errorf("no mail provider: %q", body)
	}
}
