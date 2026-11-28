SET LOCAL lock_timeout = '2s';

-- The decision-card ref a doop lock pin is held against (CHE-7538, CHE-6078 G5).
-- One row per cardRequestId: the row is the card's state and every change is one
-- compare-and-set on it, so doop never needs a transaction across two databases.
-- `missing` is the absence of a row and is never stored. epochs, r1 and r2 are the
-- epochs recorded at P5 and the receipts that opened and decided the card. No
-- foreign keys (workspace DB rules): the id is doop's cardRequestId.
CREATE TABLE IF NOT EXISTS card_ref (
    card_request_id text PRIMARY KEY CHECK (card_request_id <> ''),
    status text NOT NULL CHECK (status IN ('pending', 'open', 'pick_pending', 'decided', 'void')),
    epochs jsonb,
    r1 jsonb,
    r2 jsonb,
    pick_request_id text,
    picked_frame_id text,
    void_reason text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- pick_driver reads the cards that wait on a pick.
CREATE INDEX IF NOT EXISTS idx_card_ref_pick_pending ON card_ref (updated_at) WHERE status = 'pick_pending';
