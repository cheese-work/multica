// Package cardref is the Multica side of the doop lock pin (CHE-6078 G5, spec
// che-6078-g5-lock-pin-r1 sections 2, 4 and 5).
//
// A card ref is one row in card_ref. Every state change is one compare-and-set
// on that row, in one database, so doop and Multica never need a transaction
// across both. Doop releases a pin only when it reads `decided` or `void` here.
package cardref

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

// Status is where a card ref stands. Missing is the absence of a row and is
// never stored.
type Status string

const (
	Missing     Status = "missing"
	Pending     Status = "pending"
	Open        Status = "open"
	PickPending Status = "pick_pending"
	Decided     Status = "decided"
	Void        Status = "void"
)

// Final reports whether no CAS leaves the status again.
func (s Status) Final() bool { return s == Decided || s == Void }

// RefStatus reads where a card stands; a card with no row is Missing.
func RefStatus(ctx context.Context, db DB, id string) (Status, error) {
	var s Status
	err := db.QueryRow(ctx, `SELECT status FROM card_ref WHERE card_request_id = $1`, id).Scan(&s)
	if errors.Is(err, pgx.ErrNoRows) {
		return Missing, nil
	}
	return s, err
}

// AbortRef is A1: it ends a card whatever its state, except `decided`, and
// answers the final status. A missing row becomes a `void` tombstone; pending,
// open and pick_pending become `void`; void and decided stay. Only doop's
// force_release (F1) calls it. Reconcile calls AbortIfPending.
func AbortRef(ctx context.Context, db DB, id, reason string) (Status, error) {
	if id == "" {
		return "", ErrInvalid
	}
	var s Status
	err := db.QueryRow(ctx, `
		INSERT INTO card_ref (card_request_id, status, void_reason)
		VALUES ($1, 'void', $2)
		ON CONFLICT (card_request_id) DO UPDATE
		SET status = 'void', void_reason = EXCLUDED.void_reason, updated_at = now()
		WHERE card_ref.status IN ('pending', 'open', 'pick_pending')
		RETURNING status`, id, reason).Scan(&s)
	if errors.Is(err, pgx.ErrNoRows) { // decided or void: nothing to change
		return RefStatus(ctx, db, id)
	}
	return s, err
}

// AbortIfPending is A2, the only abort doop's reconcile makes. It is one
// conditional statement: a missing row becomes a `void` tombstone and a
// `pending` row becomes `void`. Any other state is left exactly as it is, and
// notAborted says so with the status the row holds. A slow publisher that wins
// P7 first therefore keeps its `open` card.
func AbortIfPending(ctx context.Context, db DB, id string) (status Status, notAborted bool, err error) {
	if id == "" {
		return "", false, ErrInvalid
	}
	err = db.QueryRow(ctx, `
		INSERT INTO card_ref (card_request_id, status, void_reason)
		VALUES ($1, 'void', 'reconcile: not pending')
		ON CONFLICT (card_request_id) DO UPDATE
		SET status = 'void', void_reason = EXCLUDED.void_reason, updated_at = now()
		WHERE card_ref.status = 'pending'
		RETURNING status`, id).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		status, err = RefStatus(ctx, db, id)
		return status, true, err
	}
	return status, false, err
}

// ErrInvalid is a call with an empty or malformed argument.
var ErrInvalid = errors.New("cardref: invalid argument")

var (
	ErrNoCard         = errors.New("cardref: no such card")
	ErrNotPending     = errors.New("cardref: card is not pending")
	ErrNotOpen        = errors.New("cardref: card is not open")
	ErrNotPickPending = errors.New("cardref: card is not waiting on a pick")
	ErrPickConflict   = errors.New("cardref: card already has another pick")
	ErrBadReceipt     = errors.New("cardref: receipt is not valid")
	ErrWrongEpoch     = errors.New("cardref: receipt epochs differ from the epochs recorded at P5")
)

// row is what a failed CAS reads back to say why it failed.
type row struct {
	status    Status
	pickReq   *string
	pickFrame *string
	r1MAC     *string
	r2MAC     *string
}

func readRow(ctx context.Context, db DB, id string) (*row, error) {
	var r row
	err := db.QueryRow(ctx, `
		SELECT status, pick_request_id, picked_frame_id, r1->>'mac', r2->>'mac'
		FROM card_ref WHERE card_request_id = $1`, id).Scan(&r.status, &r.pickReq, &r.pickFrame, &r.r1MAC, &r.r2MAC)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoCard
	}
	return &r, err
}

func same(p *string, s string) bool { return p != nil && *p == s }

// Create is P3: the row for a new card, `pending`. A retry, or a card that is
// already further on, answers the row's status and changes nothing.
func Create(ctx context.Context, db DB, id string) (Status, error) {
	if id == "" {
		return "", ErrInvalid
	}
	if _, err := db.Exec(ctx, `INSERT INTO card_ref (card_request_id, status) VALUES ($1, 'pending') ON CONFLICT DO NOTHING`, id); err != nil {
		return "", err
	}
	return RefStatus(ctx, db, id)
}

// RecordEpochs is P5: the epochs doop's P4 answered, written while the card is
// still `pending`. The same epochs again are a no-op; other epochs are refused.
func RecordEpochs(ctx context.Context, db DB, id string, epochs map[string]int64) error {
	if id == "" || len(epochs) == 0 {
		return ErrInvalid
	}
	raw, err := json.Marshal(epochs)
	if err != nil {
		return err
	}
	tag, err := db.Exec(ctx, `
		UPDATE card_ref SET epochs = $2::jsonb, updated_at = now()
		WHERE card_request_id = $1 AND status = 'pending' AND (epochs IS NULL OR epochs = $2::jsonb)`, id, string(raw))
	if err != nil || tag.RowsAffected() == 1 {
		return err
	}
	r, err := readRow(ctx, db, id)
	if err != nil {
		return err
	}
	if r.status != Pending {
		return ErrNotPending
	}
	return ErrWrongEpoch
}

// OpenCard is P7: pending to open, only with an R1 that doop's secret signed for
// this card and whose epochs equal the ones P5 recorded. The same R1 again
// answers the status the card has moved on to.
func OpenCard(ctx context.Context, db DB, secret, id string, r1 Receipt) (Status, error) {
	if id == "" || r1.Kind != "R1" || r1.State != "HELD" || r1.CardRequestID != id || !Verify(secret, r1) {
		return "", ErrBadReceipt
	}
	raw, err := json.Marshal(r1)
	if err != nil {
		return "", err
	}
	epochs, err := json.Marshal(r1.Epochs)
	if err != nil {
		return "", err
	}
	var s Status
	err = db.QueryRow(ctx, `
		UPDATE card_ref SET status = 'open', r1 = $3::jsonb, updated_at = now()
		WHERE card_request_id = $1 AND status = 'pending' AND epochs = $2::jsonb
		RETURNING status`, id, string(epochs), string(raw)).Scan(&s)
	if !errors.Is(err, pgx.ErrNoRows) {
		return s, err
	}
	r, err := readRow(ctx, db, id)
	switch {
	case err != nil:
		return "", err
	case r.status == Pending:
		return "", ErrWrongEpoch
	case (r.status == Open || r.status == PickPending || r.status == Decided) && same(r.r1MAC, r1.MAC):
		return r.status, nil
	}
	return "", ErrNotPending
}

// Pick is K1: open to pick_pending, recording the pick. The same pick again
// answers the status the card has moved on to; another pick is refused.
func Pick(ctx context.Context, db DB, id, pickRequestID, pickedFrameID string) (Status, error) {
	if id == "" || pickRequestID == "" || pickedFrameID == "" {
		return "", ErrInvalid
	}
	var s Status
	err := db.QueryRow(ctx, `
		UPDATE card_ref SET status = 'pick_pending', pick_request_id = $2, picked_frame_id = $3, updated_at = now()
		WHERE card_request_id = $1 AND status = 'open'
		RETURNING status`, id, pickRequestID, pickedFrameID).Scan(&s)
	if !errors.Is(err, pgx.ErrNoRows) {
		return s, err
	}
	r, err := readRow(ctx, db, id)
	switch {
	case err != nil:
		return "", err
	case r.status != PickPending && r.status != Decided:
		return "", ErrNotOpen
	case same(r.pickReq, pickRequestID) && same(r.pickFrame, pickedFrameID):
		return r.status, nil
	}
	return "", ErrPickConflict
}

// Decide is K4: pick_pending to decided, only with an R2 that doop's secret
// signed for this card, this pick and the epochs P5 recorded.
func Decide(ctx context.Context, db DB, secret, id string, r2 Receipt) (Status, error) {
	if id == "" || r2.Kind != "R2" || r2.State != "SEALED" || r2.CardRequestID != id ||
		r2.PickRequestID == "" || r2.PickedFrameID == "" || !Verify(secret, r2) {
		return "", ErrBadReceipt
	}
	raw, err := json.Marshal(r2)
	if err != nil {
		return "", err
	}
	epochs, err := json.Marshal(r2.Epochs)
	if err != nil {
		return "", err
	}
	var s Status
	err = db.QueryRow(ctx, `
		UPDATE card_ref SET status = 'decided', r2 = $5::jsonb, updated_at = now()
		WHERE card_request_id = $1 AND status = 'pick_pending'
		  AND pick_request_id = $2 AND picked_frame_id = $3 AND epochs = $4::jsonb
		RETURNING status`, id, r2.PickRequestID, r2.PickedFrameID, string(epochs), string(raw)).Scan(&s)
	if !errors.Is(err, pgx.ErrNoRows) {
		return s, err
	}
	r, err := readRow(ctx, db, id)
	switch {
	case err != nil:
		return "", err
	case r.status == Decided && same(r.r2MAC, r2.MAC):
		return Decided, nil
	case r.status != PickPending:
		return "", ErrNotPickPending
	case !same(r.pickReq, r2.PickRequestID) || !same(r.pickFrame, r2.PickedFrameID):
		return "", ErrBadReceipt
	}
	return "", ErrWrongEpoch
}

// VoidCard is C1: Multica ends a card by its own policy (it expired, or was
// withdrawn). The same CAS as A1.
func VoidCard(ctx context.Context, db DB, id, reason string) (Status, error) {
	return AbortRef(ctx, db, id, reason)
}
