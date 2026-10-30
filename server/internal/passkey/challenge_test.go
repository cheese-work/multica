package passkey

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func testPool(test *testing.T) *pgxpool.Pool {
	test.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		test.Skip("DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(pool.Close)
	return pool
}

func session(nonce []byte) ([]byte, error) { return []byte(`{"challenge":"x"}`), nil }

func newUser(test *testing.T, pool *pgxpool.Pool) pgtype.UUID {
	test.Helper()
	id := uuid.New()
	test.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM passkey_assertion_challenge WHERE user_id = $1`, id)
	})
	return pgtype.UUID{Bytes: id, Valid: true}
}

func openRows(test *testing.T, pool *pgxpool.Pool, user pgtype.UUID) int {
	test.Helper()
	var count int
	err := pool.QueryRow(context.Background(), `SELECT count(*) FROM passkey_assertion_challenge WHERE user_id = $1 AND consumed_at IS NULL AND superseded_at IS NULL`, user).Scan(&count)
	if err != nil {
		test.Fatal(err)
	}
	return count
}

func TestIssueStoresChallengeWithNonceAndFiveMinuteExpiry(test *testing.T) {
	pool := testPool(test)
	user, subject := newUser(test, pool), pgtype.UUID{Bytes: uuid.New(), Valid: true}
	before := time.Now()
	challenge, err := IssueChallenge(context.Background(), pool, user, PurposeDecisionApprove, subject, session)
	if err != nil {
		test.Fatal(err)
	}
	if len(challenge.Nonce) != 32 {
		test.Errorf("nonce is %d bytes, want 32", len(challenge.Nonce))
	}
	if ttl := challenge.ExpiresAt.Sub(before); ttl < 4*time.Minute+50*time.Second || ttl > 5*time.Minute+10*time.Second {
		test.Errorf("expiry %v from now, want 5 minutes", ttl)
	}
	row, err := db.New(pool).GetPasskeyChallenge(context.Background(), challenge.ID)
	if err != nil {
		test.Fatal(err)
	}
	var stored map[string]string
	if err := json.Unmarshal(row.SessionData, &stored); err != nil || stored["challenge"] != "x" {
		test.Errorf("session data = %s (%v)", row.SessionData, err)
	}
	if row.Purpose != "decision_approve" || row.UserID != user || row.SubjectID != subject {
		test.Errorf("stored purpose/user/subject = %s/%v/%v", row.Purpose, row.UserID, row.SubjectID)
	}
}

func TestIssueSupersedesTheOpenChallengeAcrossPurposes(test *testing.T) {
	pool := testPool(test)
	user, subject := newUser(test, pool), pgtype.UUID{Bytes: uuid.New(), Valid: true}
	first, err := IssueChallenge(context.Background(), pool, user, PurposeDecisionApprove, subject, session)
	if err != nil {
		test.Fatal(err)
	}
	second, err := IssueChallenge(context.Background(), pool, user, PurposeRevoke, subject, session)
	if err != nil {
		test.Fatal(err)
	}
	if got := openRows(test, pool, user); got != 1 {
		test.Fatalf("%d open challenges, want 1", got)
	}
	old, err := db.New(pool).GetPasskeyChallenge(context.Background(), first.ID)
	if err != nil {
		test.Fatal(err)
	}
	if !old.SupersededAt.Valid {
		test.Error("first challenge not marked superseded")
	}
	current, _ := db.New(pool).GetPasskeyChallenge(context.Background(), second.ID)
	if current.SupersededAt.Valid || current.ConsumedAt.Valid {
		test.Error("second challenge must stay open")
	}
}

func TestIssueLeavesOtherUsersChallengeOpen(test *testing.T) {
	pool := testPool(test)
	owner, other, subject := newUser(test, pool), newUser(test, pool), pgtype.UUID{Bytes: uuid.New(), Valid: true}
	if _, err := IssueChallenge(context.Background(), pool, owner, PurposeDecisionApprove, subject, session); err != nil {
		test.Fatal(err)
	}
	if _, err := IssueChallenge(context.Background(), pool, other, PurposeDecisionApprove, subject, session); err != nil {
		test.Fatal(err)
	}
	if openRows(test, pool, owner) != 1 || openRows(test, pool, other) != 1 {
		test.Error("each user must keep exactly one open challenge")
	}
}

func TestIssueFailsFastWhileAnotherTransactionHoldsTheOpenChallenge(test *testing.T) {
	pool := testPool(test)
	ctx := context.Background()
	user, subject := newUser(test, pool), pgtype.UUID{Bytes: uuid.New(), Valid: true}
	if _, err := IssueChallenge(ctx, pool, user, PurposeDecisionApprove, subject, session); err != nil {
		test.Fatal(err)
	}
	holder, err := pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := db.New(holder).LockOpenPasskeyChallenge(ctx, user); err != nil {
		test.Fatal(err)
	}
	started := time.Now()
	_, err = IssueChallenge(ctx, pool, user, PurposeRevoke, subject, session)
	if !errors.Is(err, ErrBusy) {
		test.Fatalf("err = %v, want ErrBusy", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		test.Errorf("waited %v, want an immediate refusal", elapsed)
	}
}

func TestIssueRejectsUnknownPurposeAndKeepsTheOpenChallenge(test *testing.T) {
	pool := testPool(test)
	ctx := context.Background()
	user, subject := newUser(test, pool), pgtype.UUID{Bytes: uuid.New(), Valid: true}
	if _, err := IssueChallenge(ctx, pool, user, PurposeDecisionApprove, subject, session); err != nil {
		test.Fatal(err)
	}
	if _, err := IssueChallenge(ctx, pool, user, Purpose("anything"), subject, session); err == nil {
		test.Fatal("unknown purpose accepted")
	}
	if got := openRows(test, pool, user); got != 1 {
		test.Errorf("%d open challenges after rejected purpose, want 1", got)
	}
}

func TestIssueKeepsTheOpenChallengeWhenSessionBuildFails(test *testing.T) {
	pool := testPool(test)
	ctx := context.Background()
	user, subject := newUser(test, pool), pgtype.UUID{Bytes: uuid.New(), Valid: true}
	first, err := IssueChallenge(ctx, pool, user, PurposeDecisionApprove, subject, session)
	if err != nil {
		test.Fatal(err)
	}
	boom := errors.New("build failed")
	if _, err := IssueChallenge(ctx, pool, user, PurposeRevoke, subject, func([]byte) ([]byte, error) { return nil, boom }); !errors.Is(err, boom) {
		test.Fatalf("err = %v, want build error", err)
	}
	row, _ := db.New(pool).GetPasskeyChallenge(ctx, first.ID)
	if row.SupersededAt.Valid {
		test.Error("a failed issue must not supersede the open challenge")
	}
}

func TestSecondOpenChallengeRowIsRejectedByTheDatabase(test *testing.T) {
	pool := testPool(test)
	ctx := context.Background()
	user := newUser(test, pool)
	insert := `INSERT INTO passkey_assertion_challenge (nonce, user_id, purpose, subject_id, session_data, expires_at) VALUES (decode(repeat('ab', 32), 'hex'), $1, 'decision_approve', gen_random_uuid(), '{}', now() + interval '5 minutes')`
	if _, err := pool.Exec(ctx, insert, user); err != nil {
		test.Fatal(err)
	}
	if _, err := pool.Exec(ctx, insert, user); err == nil {
		test.Fatal("two open challenges for one user were accepted")
	}
}

// slowCommit holds each transaction it begins at Commit until released, so a
// test can freeze an issuer after its insert.
type slowCommit struct {
	Beginner
	reached chan struct{}
	release chan struct{}
}

type slowTx struct {
	pgx.Tx
	owner *slowCommit
}

func (pool slowCommit) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := pool.Beginner.Begin(ctx)
	return slowTx{tx, &pool}, err
}

func (tx slowTx) Commit(ctx context.Context) error {
	close(tx.owner.reached)
	<-tx.owner.release
	return tx.Tx.Commit(ctx)
}

func TestIssueFailsFastWhileTheFirstIssuerIsStillCommitting(test *testing.T) {
	pool := testPool(test)
	user, subject := newUser(test, pool), pgtype.UUID{Bytes: uuid.New(), Valid: true}
	first := slowCommit{Beginner: pool, reached: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := IssueChallenge(context.Background(), first, user, PurposeDecisionApprove, subject, session)
		done <- err
	}()
	<-first.reached

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	_, err := IssueChallenge(ctx, pool, user, PurposeRevoke, subject, session)
	close(first.release)
	if firstErr := <-done; firstErr != nil {
		test.Fatalf("first issuer: %v", firstErr)
	}
	if !errors.Is(err, ErrBusy) {
		test.Fatalf("second issuer err = %v, want ErrBusy", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		test.Errorf("waited %v, want an immediate refusal", elapsed)
	}
	if openRows(test, pool, user) != 1 {
		test.Error("the user must keep exactly one open challenge")
	}
}

func TestChallengeExpiryIsReadAtVerificationTimeNotTransactionStart(test *testing.T) {
	pool := testPool(test)
	ctx := context.Background()
	user := newUser(test, pool)
	var id pgtype.UUID
	err := pool.QueryRow(ctx, `INSERT INTO passkey_assertion_challenge (nonce, user_id, purpose, subject_id, session_data, expires_at) VALUES (decode(repeat('ab', 32), 'hex'), $1, 'decision_approve', gen_random_uuid(), '{}', clock_timestamp() + interval '300 milliseconds') RETURNING id`, user).Scan(&id)
	if err != nil {
		test.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer tx.Rollback(ctx)
	time.Sleep(600 * time.Millisecond)
	row, err := db.New(tx).LockPasskeyChallengeNowait(ctx, id)
	if err != nil {
		test.Fatal(err)
	}
	if !row.Expired {
		test.Fatal("a challenge that expired before the lock read it must read as expired")
	}
}
