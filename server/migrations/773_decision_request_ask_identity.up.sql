SET LOCAL lock_timeout = '2s';

ALTER TABLE decision_request
    ADD COLUMN IF NOT EXISTS ask_identity text NOT NULL DEFAULT '';
