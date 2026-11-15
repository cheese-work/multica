package passkey

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/passkey/passkeytest"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// recoveryFixture is a user row (recovery resolves the person by email) that
// holds the fixture's usable passkey, an open challenge and an open ceremony.
type recoveryFixture struct {
	*fixture
	mailer *mailbox
	email  string
}

func newRecoveryFixture(test *testing.T) recoveryFixture {
	test.Helper()
	f, mailer := newEnrollFixture(test)
	email := fmt.Sprintf("recover-%d@example.test", time.Now().UnixNano())
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO "user" (id, name, email) VALUES ($1, 'Recovery test', $2)`, f.user, email); err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { f.pool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, f.user) })
	return recoveryFixture{f, mailer, email}
}

func (f recoveryFixture) recover() Recovery {
	f.test.Helper()
	recovery, err := RecoverUser(context.Background(), f.pool, f.email)
	if err != nil {
		f.test.Fatal(err)
	}
	return recovery
}

func (f recoveryFixture) state() (state string, generation int) {
	f.test.Helper()
	err := f.pool.QueryRow(context.Background(), `SELECT passkey_state, session_generation FROM "user" WHERE id = $1`, f.user).Scan(&state, &generation)
	if err != nil {
		f.test.Fatal(err)
	}
	return state, generation
}

func TestRecoveryRevokesEveryKeyKillsSessionsAndEndsOpenCeremonies(test *testing.T) {
	f := newRecoveryFixture(test)
	second := passkeytest.New(test)
	secondKey := f.enroll(f.user, second, startCount)
	if _, err := f.pool.Exec(context.Background(), `UPDATE user_passkey SET suspect_at = now() WHERE id = $1`, secondKey); err != nil {
		test.Fatal(err)
	}
	f.issue()
	ceremony := f.start()
	if n := openRows(test, f.pool, f.user); n != 1 {
		test.Fatalf("open challenges before = %d, want 1", n)
	}
	_, before := f.state()

	recovery := f.recover()

	state, after := f.state()
	if state != "recovery_required" || after != before+1 || recovery.Generation != after {
		test.Fatalf("state=%s generation %d->%d (reported %d), want recovery_required and +1", state, before, after, recovery.Generation)
	}
	if n := f.count(`SELECT count(*) FROM user_passkey WHERE user_id = $1 AND revoked_at IS NULL`, f.user); n != 0 {
		test.Fatalf("%d passkeys still live (suspect ones included)", n)
	}
	if recovery.Revoked != 2 {
		test.Fatalf("reported %d revoked, want 2", recovery.Revoked)
	}
	if n := openRows(test, f.pool, f.user); n != 0 {
		test.Fatalf("%d open challenges survived", n)
	}
	if n := f.count(`SELECT count(*) FROM passkey_enroll_ceremony WHERE id = $1 AND consumed_at IS NOT NULL`, ceremony.CeremonyID); n != 1 {
		test.Fatal("the open enrollment code survived")
	}
	if n := f.count(`SELECT count(*) FROM passkey_event WHERE user_id = $1 AND action = 'passkey_recovery' AND (details->>'generation')::int = $2 AND (details->>'revoked')::int = 2`, f.user, after); n != 1 {
		test.Fatalf("recovery audit rows = %d, want 1", n)
	}
}

func TestRecoveryOfAnUnknownEmailChangesNothing(test *testing.T) {
	f := newRecoveryFixture(test)
	if _, err := RecoverUser(context.Background(), f.pool, "nobody-"+f.email); !errors.Is(err, ErrUserNotFound) {
		test.Fatalf("err = %v, want ErrUserNotFound", err)
	}
	if state, _ := f.state(); state != "ready" || f.count(`SELECT count(*) FROM user_passkey WHERE user_id = $1 AND revoked_at IS NULL`, f.user) != 1 {
		test.Fatal("a failed recovery touched the user")
	}
}

func TestRecoveryEndsWhenTheSecondNewKeyIsEnrolled(test *testing.T) {
	f := newRecoveryFixture(test)
	f.recover()
	for enrolled := 1; enrolled <= 2; enrolled++ {
		ceremony := f.start()
		if err := f.finishAs(f.user, ceremony, f.mailer.lastCode(test), f.attest(passkeytest.New(test), ceremony, nil)); err != nil {
			test.Fatal(err)
		}
		state, _ := f.state()
		want := "recovery_required"
		if enrolled == 2 {
			want = "ready"
		}
		if state != want {
			test.Fatalf("after %d new key(s) state = %s, want %s", enrolled, state, want)
		}
	}
}

func TestUserRecoveringReadsTheStateUnderAShareLock(test *testing.T) {
	f := newRecoveryFixture(test)
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if recovering, err := UserRecovering(ctx, db.New(tx), f.user); err != nil || recovering {
		test.Fatalf("ready user: recovering=%v err=%v", recovering, err)
	}
	// FOR SHARE: a writer on the same row must wait, a second reader must not.
	other, err := f.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer other.Rollback(ctx)
	if _, err := other.Exec(ctx, `SET LOCAL lock_timeout = '200ms'`); err != nil {
		test.Fatal(err)
	}
	if _, err := other.Exec(ctx, `UPDATE "user" SET passkey_state = 'recovery_required' WHERE id = $1`, f.user); err == nil {
		test.Fatal("a writer got past the share lock")
	}
}

func TestRecoveryEndsEveryWebPushSubscriptionOfTheUserOnly(test *testing.T) {
	f := newRecoveryFixture(test)
	ctx := context.Background()
	stranger := f.pool.QueryRow(ctx, `SELECT gen_random_uuid()::text`)
	var strangerID string
	if err := stranger.Scan(&strangerID); err != nil {
		test.Fatal(err)
	}
	insertSub := func(owner any, endpoint string) string {
		var id string
		err := f.pool.QueryRow(ctx, `INSERT INTO push_subscription (user_id, endpoint, p256dh, auth, session_id, session_valid_until)
			VALUES ($1, $2, 'k', 'a', 's', now() + interval '1 day') RETURNING id::text`, owner, endpoint).Scan(&id)
		if err != nil {
			test.Fatal(err)
		}
		if _, err := f.pool.Exec(ctx, `INSERT INTO push_delivery (record_id, subscription_id, next_attempt_at) VALUES (gen_random_uuid(), $1, now())`, id); err != nil {
			test.Fatal(err)
		}
		test.Cleanup(func() {
			f.pool.Exec(ctx, `DELETE FROM push_delivery WHERE subscription_id = $1`, id)
			f.pool.Exec(ctx, `DELETE FROM push_subscription WHERE id = $1`, id)
		})
		return id
	}
	tag := fmt.Sprintf("https://push.example.test/%d", time.Now().UnixNano())
	mine := []string{insertSub(f.user, tag+"/a"), insertSub(f.user, tag+"/b")}
	theirs := insertSub(strangerID, tag+"/other")

	f.recover()

	count := func(table, column string, ids []string) (n int) {
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE `+column+` = ANY($1::uuid[])`, ids).Scan(&n); err != nil {
			test.Fatal(err)
		}
		return n
	}
	if n := count("push_subscription", "id", mine); n != 0 {
		test.Fatalf("recovered user keeps %d push subscriptions, want 0", n)
	}
	if n := count("push_delivery", "subscription_id", mine); n != 0 {
		test.Fatalf("recovered user keeps %d queued push deliveries, want 0", n)
	}
	if n := count("push_subscription", "id", []string{theirs}); n != 1 {
		test.Fatalf("another user's subscriptions = %d, want 1", n)
	}
}
