SET LOCAL lock_timeout = '2s';

ALTER TABLE decision_request DROP CONSTRAINT IF EXISTS decision_request_ping_outcome_check;
ALTER TABLE decision_request DROP COLUMN IF EXISTS ping_outcome;
