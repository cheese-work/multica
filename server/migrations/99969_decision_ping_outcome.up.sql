-- CHE-2689: how a request's ping receipt was written. 'coalesced' means the
-- ping was folded into another open ask's unread inbox item, so the worker
-- retry (which selects only a NULL ping_notified_at) never creates it.
SET LOCAL lock_timeout = '2s';

ALTER TABLE decision_request
    ADD COLUMN IF NOT EXISTS ping_outcome text;

ALTER TABLE decision_request
    DROP CONSTRAINT IF EXISTS decision_request_ping_outcome_check,
    ADD CONSTRAINT decision_request_ping_outcome_check CHECK (ping_outcome IN ('notified', 'coalesced')) NOT VALID;
