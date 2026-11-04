-- CHE-1795: the Web Push outbox. One row per (inbox record, subscription),
-- written by the inbox_item trigger in 846 so every writer is covered in the
-- record's own transaction. The worker claims due rows with a lease
-- (claimed_until) and keeps sent rows briefly. The unique key follows in 844,
-- the due-row index in 845. No foreign keys; the worker drops orphans.
CREATE TABLE IF NOT EXISTS push_delivery (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    record_id uuid NOT NULL,
    subscription_id uuid NOT NULL,
    next_attempt_at timestamptz NOT NULL,
    claimed_until timestamptz,
    attempts integer NOT NULL DEFAULT 0,
    sent_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
