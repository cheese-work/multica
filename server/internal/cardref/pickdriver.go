package cardref

import "context"

// PinClient is doop's K2 `pin_for_pick`: one transaction that seals the pin and
// answers R2. It is idempotent on the pick request id, so asking again after a
// crash on either side gives the same R2.
type PinClient interface {
	PinForPick(ctx context.Context, cardRequestID, pickRequestID, pickedFrameID string) (Receipt, error)
}

// DriveResult is what one pass did for one card.
type DriveResult struct {
	CardRequestID string
	Status        Status
	Err           error
}

// DefaultDriveLimit bounds one DriveAll pass when the caller gives none.
const DefaultDriveLimit = 100

// Drive is pick_driver for one card (spec section 5). A card that waits on a
// pick gets R2 from doop and then K4; any other card is left alone. Both sides
// may crash between the steps: the next pass asks again, and K1, K2 and K4 are
// all idempotent. It answers the card's status after the pass.
func Drive(ctx context.Context, db DB, secret string, client PinClient, id string) (Status, error) {
	r, err := readRow(ctx, db, id)
	if err == ErrNoCard {
		return Missing, nil
	}
	if err != nil {
		return "", err
	}
	if r.status != PickPending || r.pickReq == nil || r.pickFrame == nil {
		return r.status, nil
	}
	r2, err := client.PinForPick(ctx, id, *r.pickReq, *r.pickFrame)
	if err != nil {
		return PickPending, err
	}
	s, err := Decide(ctx, db, secret, id, r2)
	if err != nil {
		return PickPending, err
	}
	return s, nil
}

// DriveAll runs Drive for the pick_pending cards, oldest first, at most limit of
// them. One card that fails does not stop the others; its error is in its result.
func DriveAll(ctx context.Context, db DB, secret string, client PinClient, limit int) ([]DriveResult, error) {
	if limit <= 0 {
		limit = DefaultDriveLimit
	}
	rows, err := db.Query(ctx, `
		SELECT card_request_id FROM card_ref WHERE status = 'pick_pending'
		ORDER BY updated_at, card_request_id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]DriveResult, 0, len(ids))
	for _, id := range ids {
		s, err := Drive(ctx, db, secret, client, id)
		out = append(out, DriveResult{CardRequestID: id, Status: s, Err: err})
	}
	return out, nil
}
