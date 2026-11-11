SET LOCAL lock_timeout = '2s';

ALTER TABLE decision_request
    DROP COLUMN IF EXISTS answered_by_batch_request_id,
    DROP COLUMN IF EXISTS covers_request_ids;
