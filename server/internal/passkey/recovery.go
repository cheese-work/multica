package passkey

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	StateReady            = "ready"
	StateRecoveryRequired = "recovery_required"
	eventRecovery         = "passkey_recovery"
)

var ErrUserNotFound = errors.New("passkey: no such user")

type Recovery struct {
	UserID     pgtype.UUID
	Generation int
	Revoked    int
}

// RecoverUser is the C00 recovery command (CHE-2798 P6). One transaction puts
// the person in recovery_required, bumps session_generation so every issued
// JWT and its renewal die, revokes every passkey (suspect ones too), ends the
// open challenge and enrollment code, ends every Web Push subscription, and writes the audit row. The person
// leaves recovery when FinishEnrollment brings usable keys to two.
func RecoverUser(ctx context.Context, txs Beginner, email string) (Recovery, error) {
	tx, err := txs.Begin(ctx)
	if err != nil {
		return Recovery{}, err
	}
	defer tx.Rollback(ctx)
	// A gate check or approval in flight holds the row FOR SHARE for the length
	// of its transaction; wait for it briefly, then give up rather than queue.
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		return Recovery{}, err
	}
	queries := db.New(tx)
	userID, err := queries.LockUserForRecovery(ctx, strings.TrimSpace(email))
	if errors.Is(err, pgx.ErrNoRows) {
		return Recovery{}, ErrUserNotFound
	} else if err != nil {
		return Recovery{}, err
	}
	generation, err := queries.EnterPasskeyRecovery(ctx, userID)
	if err != nil {
		return Recovery{}, err
	}
	revoked, err := queries.RevokeAllPasskeys(ctx, userID)
	if err != nil {
		return Recovery{}, err
	}
	if err := queries.SupersedeOpenPasskeyChallenges(ctx, userID); err != nil {
		return Recovery{}, err
	}
	if err := queries.ConsumeOpenPasskeyEnrollCeremony(ctx, userID); err != nil {
		return Recovery{}, err
	}
	if err := queries.DeletePushSubscriptionsByUser(ctx, userID); err != nil {
		return Recovery{}, err
	}
	if err := writeEvent(ctx, queries, userID, eventRecovery, map[string]any{"generation": generation, "revoked": revoked}); err != nil {
		return Recovery{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Recovery{}, err
	}
	return Recovery{UserID: userID, Generation: int(generation), Revoked: int(revoked)}, nil
}

// UserRecovering reports whether the person is in recovery_required. It reads
// under the share lock a gate check takes, so the answer holds until the
// caller's transaction ends; a recovery in flight answers ErrBusy. Gated
// answers and every executable-action approval call it inside their
// transaction.
func UserRecovering(ctx context.Context, queries *db.Queries, userID pgtype.UUID) (bool, error) {
	state, err := queries.LockUserPasskeyState(ctx, userID)
	if err != nil {
		return false, busyOr(err)
	}
	return state == StateRecoveryRequired, nil
}
