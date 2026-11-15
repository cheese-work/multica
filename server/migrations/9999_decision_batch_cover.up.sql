SET LOCAL lock_timeout = '2s';

-- A batch card lists existing open per-issue cards it covers (CHE-4453).
-- covers_request_ids is set on the batch at creation. answered_by_batch_request_id
-- is stamped on each covered card when the batch is answered.
ALTER TABLE decision_request
    ADD COLUMN IF NOT EXISTS covers_request_ids uuid[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS answered_by_batch_request_id uuid;
