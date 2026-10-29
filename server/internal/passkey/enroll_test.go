package passkey

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/passkey/passkeytest"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var sixDigits = regexp.MustCompile(`\b\d{6}\b`)

type mail struct{ to, subject, body string }

// mailbox is a real-provider stand-in that keeps what it was asked to send.
type mailbox struct {
	mu   sync.Mutex
	sent []mail
	err  error
}

func (m *mailbox) SendPrivileged(to, subject, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.sent = append(m.sent, mail{to, subject, body})
	return nil
}

func (m *mailbox) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

func (m *mailbox) lastCode(test *testing.T) string {
	test.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.sent) - 1; i >= 0; i-- {
		if code := sixDigits.FindString(m.sent[i].body); code != "" {
			return code
		}
	}
	test.Fatal("no code was mailed")
	return ""
}

func (m *mailbox) subjects() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, sent := range m.sent {
		out = append(out, sent.subject)
	}
	return out
}

const testEmail = "human@example.test"

func newEnrollFixture(test *testing.T) (*fixture, *mailbox) {
	test.Helper()
	f := newFixture(test)
	mailer := &mailbox{}
	f.service.Mail, f.service.CodeKey = mailer, []byte("0123456789abcdef0123456789abcdef")
	test.Cleanup(func() {
		ctx := context.Background()
		for _, table := range []string{"passkey_enroll_ceremony", "passkey_enroll_limit", "passkey_event", "user_passkey"} {
			f.pool.Exec(ctx, `DELETE FROM `+table+` WHERE user_id = $1`, f.user)
		}
	})
	return f, mailer
}

type startedCeremony struct {
	Started
	challenge string
	excluded  []string
}

func (f *fixture) start() startedCeremony {
	f.test.Helper()
	started, err := f.service.StartEnrollment(context.Background(), f.user, testEmail)
	if err != nil {
		f.test.Fatalf("start: %v", err)
	}
	var options struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
			Exclude   []struct {
				ID string `json:"id"`
			} `json:"excludeCredentials"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(started.Options, &options); err != nil {
		f.test.Fatalf("options %s: %v", started.Options, err)
	}
	out := startedCeremony{Started: started, challenge: options.PublicKey.Challenge}
	for _, excluded := range options.PublicKey.Exclude {
		out.excluded = append(out.excluded, excluded.ID)
	}
	return out
}

func (f *fixture) attest(device *passkeytest.Authenticator, ceremony startedCeremony, mutate func(*passkeytest.Registration)) []byte {
	registration := passkeytest.Registration{Challenge: ceremony.challenge, RPID: testRPID, Origin: testOrigin, Flags: passkeytest.FlagUserPresent | passkeytest.FlagUserVerified}
	if mutate != nil {
		mutate(&registration)
	}
	return device.Register(f.test, registration)
}

func (f *fixture) finishAs(user pgtype.UUID, ceremony startedCeremony, code string, attestation []byte) error {
	return f.service.FinishEnrollment(context.Background(), user, ceremony.CeremonyID, code, attestation, testEmail)
}

func (f *fixture) credentials() int {
	f.test.Helper()
	var count int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM user_passkey WHERE user_id = $1`, f.user).Scan(&count); err != nil {
		f.test.Fatal(err)
	}
	return count
}

func (f *fixture) count(query string, args ...any) int {
	f.test.Helper()
	var count int
	if err := f.pool.QueryRow(context.Background(), query, args...).Scan(&count); err != nil {
		f.test.Fatal(err)
	}
	return count
}

func TestEnrollmentFailsClosedWithoutRealDelivery(test *testing.T) {
	cases := map[string]func(f *fixture, mailer *mailbox){
		"no provider":   func(f *fixture, _ *mailbox) { f.service.Mail = nil },
		"stdout sender": func(_ *fixture, mailer *mailbox) { mailer.err = ErrDeliveryUnavailable },
		"no code key":   func(f *fixture, _ *mailbox) { f.service.CodeKey = nil },
	}
	for name, arrange := range cases {
		test.Run(name, func(test *testing.T) {
			f, mailer := newEnrollFixture(test)
			arrange(f, mailer)
			if _, err := f.service.StartEnrollment(context.Background(), f.user, testEmail); !errors.Is(err, ErrDeliveryUnavailable) {
				test.Fatalf("err = %v, want ErrDeliveryUnavailable", err)
			}
			if n := f.count(`SELECT count(*) FROM passkey_enroll_ceremony WHERE user_id = $1`, f.user); n != 0 {
				test.Fatalf("%d ceremonies left behind", n)
			}
		})
	}
	test.Run("provider error", func(test *testing.T) {
		f, mailer := newEnrollFixture(test)
		mailer.err = errors.New("smtp: connection refused")
		if _, err := f.service.StartEnrollment(context.Background(), f.user, testEmail); !errors.Is(err, ErrDeliveryUnavailable) {
			test.Fatalf("err = %v, want ErrDeliveryUnavailable", err)
		}
		if n := f.count(`SELECT count(*) FROM passkey_enroll_ceremony WHERE user_id = $1`, f.user); n != 0 {
			test.Fatalf("%d ceremonies left behind", n)
		}
		if n := f.count(`SELECT coalesce(sum(hour_starts), 0) FROM passkey_enroll_limit WHERE user_id = $1`, f.user); n != 0 {
			test.Fatalf("an undelivered start was counted: %d", n)
		}
	})
}

func TestEnrollmentStoresTheCredentialAndNotifies(test *testing.T) {
	f, mailer := newEnrollFixture(test)
	ceremony := f.start()
	if len(ceremony.excluded) != 1 {
		test.Fatalf("excludeCredentials = %v, want the existing passkey", ceremony.excluded)
	}
	device := passkeytest.New(test)
	if err := f.finishAs(f.user, ceremony, mailer.lastCode(test), f.attest(device, ceremony, nil)); err != nil {
		test.Fatal(err)
	}
	if f.credentials() != 2 {
		test.Fatalf("credentials = %d, want 2", f.credentials())
	}
	var userID pgtype.UUID
	var signCount int64
	var flags int16
	err := f.pool.QueryRow(context.Background(), `SELECT user_id, sign_count, flags FROM user_passkey WHERE credential_id = $1`, device.CredentialID).Scan(&userID, &signCount, &flags)
	if err != nil || userID != f.user || flags&passkeytest.FlagUserVerified == 0 {
		test.Fatalf("stored credential: user=%v flags=%d err=%v", userID, flags, err)
	}
	if n := f.count(`SELECT count(*) FROM passkey_event WHERE user_id = $1 AND action = 'passkey_enrolled'`, f.user); n != 1 {
		test.Fatalf("enrolled events = %d, want 1", n)
	}
	if subjects := mailer.subjects(); len(subjects) != 2 {
		test.Fatalf("mails = %v, want the code and an enrollment notice", subjects)
	}
}

func TestEnrollmentRefusals(test *testing.T) {
	test.Run("dev code is not a code", func(test *testing.T) {
		test.Setenv("MULTICA_DEV_VERIFICATION_CODE", "888888")
		f, mailer := newEnrollFixture(test)
		ceremony := f.start()
		if mailer.lastCode(test) == "888888" {
			test.Skip("random code equals the dev code")
		}
		err := f.finishAs(f.user, ceremony, "888888", f.attest(passkeytest.New(test), ceremony, nil))
		f.wantRefusal(err, 403, "invalid_code")
		if f.credentials() != 1 {
			test.Fatal("a dev code enrolled a credential")
		}
	})
	test.Run("replay", func(test *testing.T) {
		f, mailer := newEnrollFixture(test)
		ceremony := f.start()
		code, attestation := mailer.lastCode(test), f.attest(passkeytest.New(test), ceremony, nil)
		if err := f.finishAs(f.user, ceremony, code, attestation); err != nil {
			test.Fatal(err)
		}
		f.wantRefusal(f.finishAs(f.user, ceremony, code, attestation), 409, "ceremony_used")
		if f.credentials() != 2 {
			test.Fatalf("credentials = %d, want 2", f.credentials())
		}
	})
	test.Run("another user", func(test *testing.T) {
		f, mailer := newEnrollFixture(test)
		ceremony := f.start()
		other := newUser(test, f.pool)
		f.wantRefusal(f.finishAs(other, ceremony, mailer.lastCode(test), f.attest(passkeytest.New(test), ceremony, nil)), 403, "ceremony_not_yours")
		if n := f.count(`SELECT attempts FROM passkey_enroll_ceremony WHERE id = $1`, ceremony.CeremonyID); n != 0 {
			test.Fatalf("a stranger spent %d attempts", n)
		}
	})
	test.Run("expired", func(test *testing.T) {
		f, mailer := newEnrollFixture(test)
		ceremony := f.start()
		f.pool.Exec(context.Background(), `UPDATE passkey_enroll_ceremony SET expires_at = now() - interval '1 second' WHERE id = $1`, ceremony.CeremonyID)
		f.wantRefusal(f.finishAs(f.user, ceremony, mailer.lastCode(test), f.attest(passkeytest.New(test), ceremony, nil)), 409, "ceremony_expired")
	})
	test.Run("superseded by a newer start", func(test *testing.T) {
		f, mailer := newEnrollFixture(test)
		first := f.start()
		firstCode := mailer.lastCode(test)
		f.start()
		f.wantRefusal(f.finishAs(f.user, first, firstCode, f.attest(passkeytest.New(test), first, nil)), 409, "ceremony_used")
	})
	rejected := map[string]func(*passkeytest.Registration){
		"wrong origin": func(r *passkeytest.Registration) { r.Origin = "https://evil.example.test" },
		"wrong rp id":  func(r *passkeytest.Registration) { r.RPID = "evil.example.test" },
		"no user verification": func(r *passkeytest.Registration) {
			r.Flags = passkeytest.FlagUserPresent
		},
	}
	for name, mutate := range rejected {
		test.Run(name, func(test *testing.T) {
			f, mailer := newEnrollFixture(test)
			ceremony := f.start()
			err := f.finishAs(f.user, ceremony, mailer.lastCode(test), f.attest(passkeytest.New(test), ceremony, mutate))
			f.wantRefusal(err, 403, "attestation_rejected")
			if f.credentials() != 1 {
				test.Fatal("a rejected attestation enrolled a credential")
			}
			if n := f.count(`SELECT attempts FROM passkey_enroll_ceremony WHERE id = $1`, ceremony.CeremonyID); n != 1 {
				test.Fatalf("attempts = %d, want 1", n)
			}
		})
	}
	test.Run("same credential twice", func(test *testing.T) {
		f, mailer := newEnrollFixture(test)
		ceremony := f.start()
		err := f.finishAs(f.user, ceremony, mailer.lastCode(test), f.attest(f.device, ceremony, nil))
		f.wantRefusal(err, 409, "credential_exists")
	})
}

func TestEnrollmentConcurrentFinishesStoreOneCredential(test *testing.T) {
	f, mailer := newEnrollFixture(test)
	ceremony := f.start()
	code, attestation := mailer.lastCode(test), f.attest(passkeytest.New(test), ceremony, nil)
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- f.finishAs(f.user, ceremony, code, attestation)
		}()
	}
	wg.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 || f.credentials() != 2 {
		test.Fatalf("succeeded = %d, credentials = %d, want 1 and 2", succeeded, f.credentials())
	}
}

func TestEnrollmentAttemptsAndLockout(test *testing.T) {
	f, mailer := newEnrollFixture(test)
	burn := func() startedCeremony {
		ceremony := f.start()
		good := f.attest(passkeytest.New(test), ceremony, nil)
		for range 5 {
			f.wantRefusal(f.finishAs(f.user, ceremony, "000000", good), 403, "invalid_code")
		}
		return ceremony
	}
	first := burn()
	f.wantRefusal(f.finishAs(f.user, first, mailer.lastCode(test), f.attest(passkeytest.New(test), first, nil)), 409, "ceremony_used")
	burn()
	if n := f.count(`SELECT count(*) FROM passkey_event WHERE user_id = $1 AND action = 'passkey_enroll_locked'`, f.user); n != 0 {
		test.Fatalf("locked after two failed ceremonies")
	}
	burn()
	if n := f.count(`SELECT count(*) FROM passkey_event WHERE user_id = $1 AND action = 'passkey_enroll_locked'`, f.user); n != 1 {
		test.Fatalf("lock events = %d, want 1", n)
	}
	before := mailer.count()
	_, err := f.service.StartEnrollment(context.Background(), f.user, testEmail)
	f.wantRefusal(err, 403, "enrollment_locked")
	if mailer.count() != before {
		test.Fatal("a locked start mailed a code")
	}
	var notified bool
	for _, subject := range mailer.subjects() {
		notified = notified || subject == lockedSubject
	}
	if !notified {
		test.Fatalf("no lock notice in %v", mailer.subjects())
	}
}

func TestEnrollmentStartLimits(test *testing.T) {
	test.Run("three per hour", func(test *testing.T) {
		f, _ := newEnrollFixture(test)
		for range 3 {
			f.start()
		}
		_, err := f.service.StartEnrollment(context.Background(), f.user, testEmail)
		f.wantRefusal(err, 429, "enroll_rate_limited")
		f.pool.Exec(context.Background(), `UPDATE passkey_enroll_limit SET hour_started_at = now() - interval '61 minutes' WHERE user_id = $1`, f.user)
		f.start()
	})
	test.Run("ten per day", func(test *testing.T) {
		f, _ := newEnrollFixture(test)
		f.start()
		f.pool.Exec(context.Background(), `UPDATE passkey_enroll_limit SET hour_started_at = now() - interval '2 hours', day_starts = 10 WHERE user_id = $1`, f.user)
		_, err := f.service.StartEnrollment(context.Background(), f.user, testEmail)
		f.wantRefusal(err, 429, "enroll_rate_limited")
	})
}

func TestEnrollCeremonyExpiryIsReadAtVerificationTimeNotTransactionStart(test *testing.T) {
	pool := testPool(test)
	ctx := context.Background()
	user := newUser(test, pool)
	test.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM passkey_enroll_ceremony WHERE user_id = $1`, user)
	})
	var id pgtype.UUID
	err := pool.QueryRow(ctx, `INSERT INTO passkey_enroll_ceremony (user_id, code_hmac, session_data, expires_at) VALUES ($1, decode(repeat('ab', 32), 'hex'), '{}', clock_timestamp() + interval '300 milliseconds') RETURNING id`, user).Scan(&id)
	if err != nil {
		test.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer tx.Rollback(ctx)
	time.Sleep(600 * time.Millisecond)
	row, err := db.New(tx).LockPasskeyEnrollCeremonyNowait(ctx, id)
	if err != nil {
		test.Fatal(err)
	}
	if !row.Expired {
		test.Fatal("a ceremony that expired before the lock read it must read as expired")
	}
}
