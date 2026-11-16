SET LOCAL lock_timeout = '2s';

ALTER TABLE decision_request DROP COLUMN IF EXISTS batch_id;
