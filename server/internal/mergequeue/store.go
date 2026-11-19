package mergequeue

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Execer is the part of a pool or transaction the writers need.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// ErrStaleState is returned when the row is no longer in the state the caller
// read, so the guarded update touched nothing.
var ErrStaleState = errors.New("mergequeue: row is not in the expected state")

// TransitionEpoch moves an epoch from one state to another. It is the only
// writer of merge_queue_epoch.state: the edge must be in the table, and the
// update is guarded by the state the caller read.
func TransitionEpoch(ctx context.Context, db Execer, id uuid.UUID, from, to State) error {
	if err := Check(from, to); err != nil {
		return err
	}
	tag, err := db.Exec(ctx, `
		UPDATE merge_queue_epoch
		SET state = $3,
		    resolved_at = CASE WHEN $3 = 'resolved' THEN now() ELSE resolved_at END,
		    updated_at = now()
		WHERE id = $1 AND state = $2`, id, string(from), string(to))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrStaleState
	}
	return nil
}

// Delivery values of an intake row (plan 4.3).
const (
	DeliveryPending = "pending"
	DeliveryHeld    = "held"
	DeliveryRunSent = "run_sent"
)

// SetDelivery sets an intake row's delivery once, from pending. It reports
// whether this call set it; a second call, or one for a value other than held
// or run_sent, changes nothing.
func SetDelivery(ctx context.Context, db Execer, id uuid.UUID, delivery, reason string) (bool, error) {
	if delivery != DeliveryHeld && delivery != DeliveryRunSent {
		return false, fmt.Errorf("mergequeue: delivery must be %s or %s, got %q", DeliveryHeld, DeliveryRunSent, delivery)
	}
	tag, err := db.Exec(ctx, `
		UPDATE merge_queue_intake
		SET delivery = $2, delivery_reason = NULLIF($3, ''), updated_at = now()
		WHERE id = $1 AND delivery = 'pending'`, id, delivery, reason)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
