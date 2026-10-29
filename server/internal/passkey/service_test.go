package passkey

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/passkey/passkeytest"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	testRPID    = "app.example.test"
	testOrigin  = "https://app.example.test"
	testPayload = "0a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f9"
	startCount  = 10
)

type fixture struct {
	test    *testing.T
	pool    *pgxpool.Pool
	service *Service
	user    pgtype.UUID
	action  pgtype.UUID
	device  *passkeytest.Authenticator
	key     pgtype.UUID // user_passkey.id
}

func newFixture(test *testing.T) *fixture {
	test.Helper()
	pool := testPool(test)
	relyingParty, err := FromEnv(func(name string) string {
		return map[string]string{"MULTICA_WEBAUTHN_RP_ID": testRPID, "MULTICA_WEBAUTHN_ORIGINS": testOrigin}[name]
	})
	if err != nil || relyingParty == nil {
		test.Fatalf("relying party: %v %v", relyingParty, err)
	}
	f := &fixture{test: test, pool: pool, service: &Service{WebAuthn: relyingParty, Queries: db.New(pool), Tx: pool}, user: newUser(test, pool), action: pgtype.UUID{Bytes: uuid.New(), Valid: true}, device: passkeytest.New(test)}
	f.key = f.enroll(f.user, f.device, startCount)
	return f
}

func (f *fixture) enroll(user pgtype.UUID, device *passkeytest.Authenticator, signCount int64) pgtype.UUID {
	f.test.Helper()
	var id pgtype.UUID
	err := f.pool.QueryRow(context.Background(),
		`INSERT INTO user_passkey (user_id, credential_id, public_key, sign_count, flags) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		user, device.CredentialID, device.COSEPublicKey(f.test), signCount, passkeytest.FlagUserPresent|passkeytest.FlagUserVerified).Scan(&id)
	if err != nil {
		f.test.Fatal(err)
	}
	f.test.Cleanup(func() {
		f.pool.Exec(context.Background(), `DELETE FROM user_passkey WHERE id = $1`, id)
	})
	return id
}

func (f *fixture) issue() (Issued, string) {
	f.test.Helper()
	return f.issueFor(f.user, f.action, testPayload)
}

func (f *fixture) issueFor(user, action pgtype.UUID, payload string) (Issued, string) {
	f.test.Helper()
	issued, err := f.service.IssueApproval(context.Background(), user, action, payload)
	if err != nil {
		f.test.Fatal(err)
	}
	return issued, challengeOf(f.test, issued)
}

// challengeOf reads the challenge a client would sign out of the issued options.
func challengeOf(test *testing.T, issued Issued) string {
	test.Helper()
	var options struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(issued.Options, &options); err != nil {
		test.Fatalf("options %s: %v", issued.Options, err)
	}
	return options.PublicKey.Challenge
}

func (f *fixture) sign(issued Issued, challenge string, counter uint32, mutate func(*passkeytest.Signing)) json.RawMessage {
	f.test.Helper()
	return f.signAs(f.device, issued, challenge, counter, mutate)
}

func (f *fixture) signAs(device *passkeytest.Authenticator, issued Issued, challenge string, counter uint32, mutate func(*passkeytest.Signing)) json.RawMessage {
	f.test.Helper()
	s := passkeytest.Signing{Challenge: challenge, RPID: testRPID, Origin: testOrigin, Flags: passkeytest.FlagUserPresent | passkeytest.FlagUserVerified, Counter: counter, UserID: f.user.Bytes[:]}
	if mutate != nil {
		mutate(&s)
	}
	envelope, err := json.Marshal(map[string]any{"challenge_id": uuid.UUID(issued.ChallengeID.Bytes).String(), "credential": device.Assert(f.test, s)})
	if err != nil {
		f.test.Fatal(err)
	}
	return envelope
}

// verify runs the verifier in its own transaction, as the approval does.
func (f *fixture) verify(assertion json.RawMessage) (pgtype.UUID, error) {
	return f.verifyAs(f.user, f.action, testPayload, assertion)
}

func (f *fixture) verifyAs(user, action pgtype.UUID, payload string, assertion json.RawMessage) (pgtype.UUID, error) {
	f.test.Helper()
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		f.test.Fatal(err)
	}
	defer tx.Rollback(ctx)
	credential, err := f.service.VerifyApproval(ctx, db.New(tx), user, action, payload, assertion)
	if err != nil {
		return credential, err
	}
	if err := tx.Commit(ctx); err != nil {
		f.test.Fatal(err)
	}
	return credential, nil
}

func (f *fixture) wantRefusal(err error, status int, code string) Refusal {
	f.test.Helper()
	var refusal Refusal
	if !errors.As(err, &refusal) {
		f.test.Fatalf("err = %v, want a Refusal %d %s", err, status, code)
	}
	if refusal.Status != status || !strings.HasPrefix(refusal.Message, code+":") {
		f.test.Fatalf("refusal = %d %q, want %d %s", refusal.Status, refusal.Message, status, code)
	}
	return refusal
}

func (f *fixture) stored() (count int64, suspect bool) {
	f.test.Helper()
	err := f.pool.QueryRow(context.Background(), `SELECT sign_count, suspect_at IS NOT NULL FROM user_passkey WHERE id = $1`, f.key).Scan(&count, &suspect)
	if err != nil {
		f.test.Fatal(err)
	}
	return
}

func TestVerifyAcceptsAValidAssertionAndAdvancesTheCounter(test *testing.T) {
	f := newFixture(test)
	issued, challenge := f.issue()
	credential, err := f.verify(f.sign(issued, challenge, startCount+1, nil))
	if err != nil {
		test.Fatal(err)
	}
	if credential != f.key {
		test.Errorf("credential = %v, want %v", credential, f.key)
	}
	if count, suspect := f.stored(); count != startCount+1 || suspect {
		test.Errorf("stored counter %d suspect %v, want %d false", count, suspect, startCount+1)
	}
	row, _ := f.service.Queries.GetPasskeyChallenge(context.Background(), issued.ChallengeID)
	if !row.ConsumedAt.Valid {
		test.Error("challenge not consumed")
	}
}

func TestIssueBindsTheChallengeToActionPayloadAndNonce(test *testing.T) {
	f := newFixture(test)
	issued, challenge := f.issue()
	row, err := f.service.Queries.GetPasskeyChallenge(context.Background(), issued.ChallengeID)
	if err != nil {
		test.Fatal(err)
	}
	hash := sha256.New()
	hash.Write(f.action.Bytes[:])
	hash.Write([]byte(testPayload))
	hash.Write(row.Nonce)
	if want := base64.RawURLEncoding.EncodeToString(hash.Sum(nil)); challenge != want {
		test.Errorf("challenge = %s, want sha256(action_id || payload_sha256 || nonce) = %s", challenge, want)
	}
	var options struct {
		PublicKey struct {
			UserVerification string                `json:"userVerification"`
			Allow            []struct{ ID string } `json:"allowCredentials"`
			RPID             string                `json:"rpId"`
		} `json:"publicKey"`
	}
	json.Unmarshal(issued.Options, &options)
	if options.PublicKey.UserVerification != "required" || len(options.PublicKey.Allow) != 1 || options.PublicKey.RPID != testRPID {
		test.Errorf("options = %s", issued.Options)
	}
	if row.Purpose != "decision_approve" || row.SubjectID != f.action {
		test.Errorf("challenge purpose/subject = %s/%v", row.Purpose, row.SubjectID)
	}
}

func TestIssueWithoutAPasskeyIsRefused(test *testing.T) {
	f := newFixture(test)
	stranger := newUser(test, f.pool)
	if _, err := f.service.IssueApproval(context.Background(), stranger, f.action, testPayload); !errors.Is(err, ErrNoPasskey) {
		test.Fatalf("err = %v, want ErrNoPasskey", err)
	}
}

func TestVerifyRefusesAMissingAssertion(test *testing.T) {
	f := newFixture(test)
	f.issue()
	for _, assertion := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(`{}`), json.RawMessage(`"x"`)} {
		_, err := f.verify(assertion)
		f.wantRefusal(err, http.StatusForbidden, "assertion_required")
	}
}

func TestVerifyRefusesAChallengeIssuedForAnotherPayload(test *testing.T) {
	f := newFixture(test)
	issued, challenge := f.issue()
	other := strings.Repeat("ab", 32)
	_, err := f.verifyAs(f.user, f.action, other, f.sign(issued, challenge, startCount+1, nil))
	f.wantRefusal(err, http.StatusForbidden, "assertion_rejected")
	if count, _ := f.stored(); count != startCount {
		test.Errorf("counter moved to %d on a refused assertion", count)
	}
}

func TestVerifyRefusesAChallengeIssuedForAnotherAction(test *testing.T) {
	f := newFixture(test)
	issued, challenge := f.issue()
	_, err := f.verifyAs(f.user, pgtype.UUID{Bytes: uuid.New(), Valid: true}, testPayload, f.sign(issued, challenge, startCount+1, nil))
	f.wantRefusal(err, http.StatusForbidden, "assertion_rejected")
}

func TestVerifyRefusesAnotherUsersChallenge(test *testing.T) {
	f := newFixture(test)
	issued, challenge := f.issue()
	stranger := newUser(test, f.pool)
	_, err := f.verifyAs(stranger, f.action, testPayload, f.sign(issued, challenge, startCount+1, nil))
	f.wantRefusal(err, http.StatusForbidden, "assertion_rejected")
}

func TestVerifyRefusesAReplayedChallenge(test *testing.T) {
	f := newFixture(test)
	issued, challenge := f.issue()
	assertion := f.sign(issued, challenge, startCount+1, nil)
	if _, err := f.verify(assertion); err != nil {
		test.Fatal(err)
	}
	_, err := f.verify(assertion)
	f.wantRefusal(err, http.StatusForbidden, "assertion_rejected")
}

func TestVerifyRefusesAnExpiredChallenge(test *testing.T) {
	f := newFixture(test)
	issued, challenge := f.issue()
	if _, err := f.pool.Exec(context.Background(), `UPDATE passkey_assertion_challenge SET expires_at = now() - interval '1 second' WHERE id = $1`, issued.ChallengeID); err != nil {
		test.Fatal(err)
	}
	_, err := f.verify(f.sign(issued, challenge, startCount+1, nil))
	f.wantRefusal(err, http.StatusForbidden, "challenge_expired")
}

func TestVerifyRefusesASupersededChallengeWithoutSuspectingTheCredential(test *testing.T) {
	f := newFixture(test)
	first, firstChallenge := f.issue()
	delayed := f.sign(first, firstChallenge, startCount+1, nil)
	second, secondChallenge := f.issue()
	if _, err := f.verify(f.sign(second, secondChallenge, startCount+2, nil)); err != nil {
		test.Fatal(err)
	}
	_, err := f.verify(delayed)
	f.wantRefusal(err, http.StatusForbidden, "challenge_superseded")
	if count, suspect := f.stored(); count != startCount+2 || suspect {
		test.Errorf("counter %d suspect %v, want %d false", count, suspect, startCount+2)
	}
	third, thirdChallenge := f.issue()
	if _, err := f.verify(f.sign(third, thirdChallenge, startCount+3, nil)); err != nil {
		test.Errorf("a fresh touch must succeed: %v", err)
	}
}

func TestVerifyRefusesARevokedCredential(test *testing.T) {
	f := newFixture(test)
	issued, challenge := f.issue()
	if _, err := f.pool.Exec(context.Background(), `UPDATE user_passkey SET revoked_at = now() WHERE id = $1`, f.key); err != nil {
		test.Fatal(err)
	}
	_, err := f.verify(f.sign(issued, challenge, startCount+1, nil))
	f.wantRefusal(err, http.StatusForbidden, "assertion_rejected")
}

func TestVerifyRefusesAnotherUsersCredential(test *testing.T) {
	f := newFixture(test)
	issued, challenge := f.issue()
	stranger := passkeytest.New(test)
	f.enroll(newUser(test, f.pool), stranger, 0)
	f.device = stranger
	_, err := f.verify(f.sign(issued, challenge, 1, nil))
	f.wantRefusal(err, http.StatusForbidden, "assertion_rejected")
}

func TestVerifyRefusesMissingUserVerificationAndWrongOrigin(test *testing.T) {
	for name, mutate := range map[string]func(*passkeytest.Signing){
		"user verification missing": func(s *passkeytest.Signing) { s.Flags = passkeytest.FlagUserPresent },
		"wrong origin":              func(s *passkeytest.Signing) { s.Origin = "https://evil.example.test" },
		"wrong relying party":       func(s *passkeytest.Signing) { s.RPID = "evil.example.test" },
	} {
		test.Run(name, func(test *testing.T) {
			f := newFixture(test)
			issued, challenge := f.issue()
			_, err := f.verify(f.sign(issued, challenge, startCount+1, mutate))
			f.wantRefusal(err, http.StatusForbidden, "assertion_rejected")
			if count, _ := f.stored(); count != startCount {
				test.Errorf("counter moved to %d", count)
			}
		})
	}
}

func TestVerifyRefusesASignCountRegressionAndNamesTheCredential(test *testing.T) {
	f := newFixture(test)
	issued, challenge := f.issue()
	_, err := f.verify(f.sign(issued, challenge, startCount, nil))
	refusal := f.wantRefusal(err, http.StatusForbidden, "assertion_rejected")
	if refusal.Suspect != f.key {
		test.Fatalf("suspect = %v, want the signing credential %v", refusal.Suspect, f.key)
	}
	if _, suspect := f.stored(); suspect {
		test.Error("the verifier must leave the mark to the caller, after its rollback")
	}
	if err := f.service.MarkSuspect(context.Background(), refusal.Suspect); err != nil {
		test.Fatal(err)
	}
	if _, suspect := f.stored(); !suspect {
		test.Error("MarkSuspect did not mark the credential")
	}
	if _, err := f.service.IssueApproval(context.Background(), f.user, f.action, testPayload); !errors.Is(err, ErrNoPasskey) {
		test.Errorf("a suspect credential must not be offered again: %v", err)
	}
}

func TestOnlyVerifyAssertionCallsLoginValidation(test *testing.T) {
	forbidden := []string{".ValidateLogin(", ".FinishLogin(", ".ValidateDiscoverableLogin(", ".FinishDiscoverableLogin(", ".ValidatePasskeyLogin(", ".FinishPasskeyLogin("}
	root := filepath.Join("..", "..")
	var offenders []string
	filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.HasPrefix(path, filepath.Join(root, "pkg", "db")) {
			return nil
		}
		source, _ := os.ReadFile(path)
		for _, call := range forbidden {
			if strings.Contains(string(source), call) && filepath.Base(path) != "verify.go" {
				offenders = append(offenders, path+" "+call)
			}
		}
		return nil
	})
	if len(offenders) > 0 {
		test.Errorf("login validation outside verify.go: %v", offenders)
	}
	source, err := os.ReadFile("verify.go")
	if err != nil || !strings.Contains(string(source), ".ValidateLogin(") {
		test.Errorf("verify.go must hold the one validation call: %v", err)
	}
}

func TestFromEnvRequiresBothSettingsAndHTTPSOrigins(test *testing.T) {
	env := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}
	if wa, err := FromEnv(env(nil)); wa != nil || err != nil {
		test.Errorf("unset: %v %v, want nil nil", wa, err)
	}
	if wa, err := FromEnv(env(map[string]string{"MULTICA_WEBAUTHN_RP_ID": testRPID})); wa != nil || err != nil {
		test.Errorf("origins unset: %v %v, want nil nil", wa, err)
	}
	if wa, err := FromEnv(env(map[string]string{"MULTICA_WEBAUTHN_ORIGINS": testOrigin})); wa != nil || err != nil {
		test.Errorf("rp id unset: %v %v, want nil nil", wa, err)
	}
	for _, origin := range []string{"http://app.example.test", "app.example.test", "https://app.example.test/path", "https://*.example.test", "https://app.example.test,http://x.test"} {
		if wa, err := FromEnv(env(map[string]string{"MULTICA_WEBAUTHN_RP_ID": testRPID, "MULTICA_WEBAUTHN_ORIGINS": origin})); wa != nil || err == nil {
			test.Errorf("origin %q accepted", origin)
		}
	}
	if wa, err := FromEnv(env(map[string]string{"MULTICA_WEBAUTHN_RP_ID": testRPID, "MULTICA_WEBAUTHN_ORIGINS": testOrigin + ", https://other.example.test"})); wa == nil || err != nil {
		test.Errorf("two https origins: %v %v", wa, err)
	}
}

// A busy refusal aborts the approval transaction, which rolls the verifier
// back. The browser retries the same signed assertion on the still-open
// challenge: it must succeed once and move the counter by exactly one.
func TestVerifyAcceptsTheSameAssertionAgainAfterARolledBackAttempt(test *testing.T) {
	f := newFixture(test)
	issued, challenge := f.issue()
	assertion := f.sign(issued, challenge, startCount+1, nil)

	ctx := context.Background()
	aborted, err := f.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := f.service.VerifyApproval(ctx, db.New(aborted), f.user, f.action, testPayload, assertion); err != nil {
		test.Fatal(err)
	}
	aborted.Rollback(ctx)
	if count, _ := f.stored(); count != startCount {
		test.Fatalf("a rolled-back attempt moved the counter to %d", count)
	}

	if _, err := f.verify(assertion); err != nil {
		test.Fatalf("retry: %v", err)
	}
	if count, suspect := f.stored(); count != startCount+1 || suspect {
		test.Fatalf("stored counter %d suspect %v, want %d false", count, suspect, startCount+1)
	}
	_, err = f.verify(assertion)
	f.wantRefusal(err, http.StatusForbidden, "assertion_rejected")
	if count, _ := f.stored(); count != startCount+1 {
		test.Fatalf("a replay moved the counter to %d", count)
	}
}
