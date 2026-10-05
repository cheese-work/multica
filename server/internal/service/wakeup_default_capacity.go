package service

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// WakeupDefaultCapacityReached is the visible reason of an instance that its
// full default pool left unapplied.
const WakeupDefaultCapacityReached = "Default capacity reached"

// DefaultWakeupAdmission is the outcome of applying a default-derived
// instance. Pool names the exhausted ceiling: "issue", "scope" or "workspace".
type DefaultWakeupAdmission struct {
	Applied bool
	Pool    string
	Reason  string
}

// ApplyDefaultWakeupInstance enables one default-derived instance inside tx.
// A full default pool is an outcome, not an error: the instance stays
// disabled with the visible reason and tx remains usable, so a source write
// or local-rule creation sharing it never fails on default capacity.
func ApplyDefaultWakeupInstance(ctx context.Context, tx pgx.Tx, id pgtype.UUID) (DefaultWakeupAdmission, error) {
	attempt, err := tx.Begin(ctx) // savepoint: the guard's exception aborts only this
	if err != nil {
		return DefaultWakeupAdmission{}, err
	}
	n, err := db.New(attempt).EnableDefaultWakeupInstance(ctx, id)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.ConstraintName == "issue_wakeup_default_capacity" {
		if err := attempt.Rollback(ctx); err != nil {
			return DefaultWakeupAdmission{}, err
		}
		if err := db.New(tx).MarkDefaultWakeupCapacityReached(ctx, db.MarkDefaultWakeupCapacityReachedParams{ID: id, Reason: pgtype.Text{String: WakeupDefaultCapacityReached, Valid: true}}); err != nil {
			return DefaultWakeupAdmission{}, err
		}
		return DefaultWakeupAdmission{Pool: pg.Detail, Reason: WakeupDefaultCapacityReached}, nil
	}
	if err != nil {
		_ = attempt.Rollback(ctx)
		return DefaultWakeupAdmission{}, err
	}
	if err := attempt.Commit(ctx); err != nil {
		return DefaultWakeupAdmission{}, err
	}
	if n == 0 {
		return DefaultWakeupAdmission{}, pgx.ErrNoRows
	}
	return DefaultWakeupAdmission{Applied: true}, nil
}
