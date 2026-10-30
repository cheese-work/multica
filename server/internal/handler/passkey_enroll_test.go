package handler

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/passkey"
	"github.com/multica-ai/multica/server/internal/passkey/passkeytest"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/featureflag"
)

var enrollCode = regexp.MustCompile(`\b\d{6}\b`)

// codeMailbox is a real-provider stand-in that keeps the mail it was given.
type codeMailbox struct {
	mu   sync.Mutex
	body []string
}

func (in *codeMailbox) SendPrivileged(_, _, body string) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.body = append(in.body, body)
	return nil
}

func (in *codeMailbox) code(test *testing.T) string {
	test.Helper()
	in.mu.Lock()
	defer in.mu.Unlock()
	for _, body := range in.body {
		if code := enrollCode.FindString(body); code != "" {
			return code
		}
	}
	test.Fatal("no code was mailed")
	return ""
}

// enrollHandler is a handler with passkeys on, enrollment open and mail going
// to mailer.
func enrollHandler(test *testing.T, enrollment bool, mailer passkey.Mailer) *Handler {
	test.Helper()
	handler := passkeyHandler(test)
	handler.Passkeys.Mail, handler.Passkeys.CodeKey = mailer, []byte("0123456789abcdef0123456789abcdef")
	provider := featureflag.NewStaticProvider()
	provider.LoadRules(map[string]featureflag.Rule{featureflags.PasskeyEnrollment: {Default: enrollment}})
	handler.FeatureFlags = featureflag.NewService(provider)
	return handler
}

func enrollUser(test *testing.T) string {
	test.Helper()
	user := dbfx.User(test, "Enrolling human", "enroll-"+uuid.NewString()+"@test.invalid")
	for _, table := range []string{"passkey_enroll_ceremony", "passkey_enroll_limit", "passkey_event", "user_passkey"} {
		dbfx.Cleanup(test, `DELETE FROM `+table+` WHERE user_id = $1`, user)
	}
	return user
}

func enrollRequest(path, user, source string, body any) *http.Request {
	request := newRequest(http.MethodPost, path, body)
	request.Header.Set("X-User-ID", user)
	request.Header.Set("X-Actor-Source", source)
	return request
}

type startedEnrollment struct {
	CeremonyID string `json:"ceremony_id"`
	Options    struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	} `json:"options"`
}

func startEnrollment(test *testing.T, handler *Handler, user string) startedEnrollment {
	test.Helper()
	var started startedEnrollment
	decodeJSON(test, testutil.Call(test, handler.StartPasskeyEnrollment, enrollRequest("/api/passkeys/enroll/start", user, "interactive_jwt", nil)).Want(http.StatusOK).Text(), &started)
	return started
}

func finishBody(test *testing.T, started startedEnrollment, code string) map[string]any {
	test.Helper()
	attestation := passkeytest.New(test).Register(test, passkeytest.Registration{
		Challenge: started.Options.PublicKey.Challenge, RPID: passkeyRPID, Origin: passkeyOrigin,
		Flags: passkeytest.FlagUserPresent | passkeytest.FlagUserVerified,
	})
	return map[string]any{"ceremony_id": started.CeremonyID, "code": code, "attestation": json.RawMessage(attestation)}
}

func credentialCount(test *testing.T, user string) int {
	test.Helper()
	var count int
	dbfx.QueryRow(test, `SELECT count(*) FROM user_passkey WHERE user_id = $1`, user).Scan(&count)
	return count
}

func TestPasskeyEnrollmentIsClosedByDefault(test *testing.T) {
	user := enrollUser(test)
	handler := enrollHandler(test, false, &codeMailbox{})
	body := testutil.Call(test, handler.StartPasskeyEnrollment, enrollRequest("/api/passkeys/enroll/start", user, "interactive_jwt", nil)).Want(http.StatusForbidden).Text()
	if !strings.Contains(body, "passkey_enrollment_disabled") {
		test.Fatalf("flag off: %q", body)
	}
	finish := enrollRequest("/api/passkeys/enroll/finish", user, "interactive_jwt", map[string]any{"ceremony_id": uuid.NewString(), "code": "123456", "attestation": map[string]any{}})
	testutil.Call(test, handler.FinishPasskeyEnrollment, finish).Want(http.StatusForbidden)
	testutil.Call(test, testHandler.StartPasskeyEnrollment, enrollRequest("/api/passkeys/enroll/start", user, "interactive_jwt", nil)).Want(http.StatusForbidden)
}

func TestPasskeyEnrollmentNeedsAnInteractiveSession(test *testing.T) {
	user := enrollUser(test)
	handler := enrollHandler(test, true, &codeMailbox{})
	for _, source := range []string{"cli_jwt", "pat", "task_token", "cloud_pat", ""} {
		testutil.Call(test, handler.StartPasskeyEnrollment, enrollRequest("/api/passkeys/enroll/start", user, source, nil)).Want(http.StatusForbidden)
	}
}

func TestPasskeyEnrollmentFailsClosedWithoutRealMail(test *testing.T) {
	cases := map[string]passkey.Mailer{"no mailer": nil, "stdout fallback": &service.EmailService{}}
	for name, mailer := range cases {
		test.Run(name, func(test *testing.T) {
			user := enrollUser(test)
			handler := enrollHandler(test, true, mailer)
			body := testutil.Call(test, handler.StartPasskeyEnrollment, enrollRequest("/api/passkeys/enroll/start", user, "interactive_jwt", nil)).Want(http.StatusServiceUnavailable).Text()
			if !strings.Contains(body, "stepup_delivery_unavailable") {
				test.Fatalf("body %q", body)
			}
			var ceremonies int
			dbfx.QueryRow(test, `SELECT count(*) FROM passkey_enroll_ceremony WHERE user_id = $1`, user).Scan(&ceremonies)
			if ceremonies != 0 {
				test.Fatalf("%d ceremonies left behind", ceremonies)
			}
		})
	}
}

func TestPasskeyEnrollmentStoresTheCredential(test *testing.T) {
	user := enrollUser(test)
	mailbox := &codeMailbox{}
	handler := enrollHandler(test, true, mailbox)
	started := startEnrollment(test, handler, user)
	finish := func(body map[string]any) *testutil.Response {
		return testutil.Call(test, handler.FinishPasskeyEnrollment, enrollRequest("/api/passkeys/enroll/finish", user, "interactive_jwt", body))
	}
	body := finishBody(test, started, mailbox.code(test))
	finish(body).Want(http.StatusOK)
	if credentialCount(test, user) != 1 {
		test.Fatalf("credentials = %d, want 1", credentialCount(test, user))
	}
	finish(body).Want(http.StatusConflict)
	if credentialCount(test, user) != 1 {
		test.Fatal("a replay stored a second credential")
	}
}

func TestPasskeyEnrollmentRefusals(test *testing.T) {
	test.Run("wrong code", func(test *testing.T) {
		user := enrollUser(test)
		mailbox := &codeMailbox{}
		handler := enrollHandler(test, true, mailbox)
		started := startEnrollment(test, handler, user)
		body := testutil.Call(test, handler.FinishPasskeyEnrollment, enrollRequest("/api/passkeys/enroll/finish", user, "interactive_jwt", finishBody(test, started, "000000"))).Want(http.StatusForbidden).Text()
		if !strings.Contains(body, "invalid_code") || credentialCount(test, user) != 0 {
			test.Fatalf("wrong code: %q", body)
		}
	})
	test.Run("another user", func(test *testing.T) {
		user, stranger := enrollUser(test), enrollUser(test)
		mailbox := &codeMailbox{}
		handler := enrollHandler(test, true, mailbox)
		started := startEnrollment(test, handler, user)
		testutil.Call(test, handler.FinishPasskeyEnrollment, enrollRequest("/api/passkeys/enroll/finish", stranger, "interactive_jwt", finishBody(test, started, mailbox.code(test)))).Want(http.StatusForbidden)
		if credentialCount(test, user)+credentialCount(test, stranger) != 0 {
			test.Fatal("a stranger enrolled a credential")
		}
	})
	test.Run("malformed body", func(test *testing.T) {
		user := enrollUser(test)
		handler := enrollHandler(test, true, &codeMailbox{})
		testutil.Call(test, handler.FinishPasskeyEnrollment, enrollRequest("/api/passkeys/enroll/finish", user, "interactive_jwt", map[string]any{"ceremony_id": "nope"})).Want(http.StatusBadRequest)
	})
}
