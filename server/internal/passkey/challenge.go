package passkey

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type Purpose string

const (
	PurposeDecisionApprove Purpose = "decision_approve"
	PurposeRevoke          Purpose = "passkey_revoke"
)

const ChallengeTTL = 5 * time.Minute

// ErrBusy: another request holds or just took the user's open challenge. The
// caller answers 409 busy_retry; nothing waits.
var ErrBusy = errors.New("passkey: challenge busy")

type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

type Challenge struct {
	ID        pgtype.UUID
	Nonce     []byte
	ExpiresAt time.Time
}

// IssueChallenge opens an assertion challenge for the user and supersedes the
// one they had open, whatever its purpose. build turns the fresh nonce into
// the stored session data. The swap is one transaction, so a failure keeps the
// old challenge usable.
func IssueChallenge(ctx context.Context, pool Beginner, userID pgtype.UUID, purpose Purpose, subjectID pgtype.UUID, build func(nonce []byte) ([]byte, error)) (Challenge, error) {
	if purpose != PurposeDecisionApprove && purpose != PurposeRevoke {
		return Challenge{}, fmt.Errorf("passkey: unknown purpose %q", purpose)
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return Challenge{}, err
	}
	sessionData, err := build(nonce)
	if err != nil {
		return Challenge{}, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Challenge{}, err
	}
	defer tx.Rollback(ctx)
	queries := db.New(tx)
	if free, err := queries.TryLockPasskeyChallengeIssue(ctx, userID); err != nil {
		return Challenge{}, err
	} else if !free {
		return Challenge{}, ErrBusy
	}
	open, err := queries.LockOpenPasskeyChallenge(ctx, userID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return Challenge{}, busyOr(err)
	default:
		if err := queries.SupersedePasskeyChallenge(ctx, open.ID); err != nil {
			return Challenge{}, err
		}
	}
	row, err := queries.InsertPasskeyChallenge(ctx, db.InsertPasskeyChallengeParams{
		Nonce: nonce, UserID: userID, Purpose: string(purpose), SubjectID: subjectID, SessionData: sessionData,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(ChallengeTTL), Valid: true},
	})
	if err != nil {
		return Challenge{}, busyOr(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Challenge{}, err
	}
	return Challenge{ID: row.ID, Nonce: nonce, ExpiresAt: row.ExpiresAt.Time}, nil
}

// busyOr maps a lock refusal (55P03) or a lost race on the one-open-challenge
// index (23505) to ErrBusy.
func busyOr(err error) error {
	var pgError *pgconn.PgError
	if errors.As(err, &pgError) && (pgError.Code == "55P03" || pgError.Code == "23505") {
		return ErrBusy
	}
	return err
}
