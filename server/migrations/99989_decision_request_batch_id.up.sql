SET LOCAL lock_timeout = '2s';

-- One card for a batch of proposals (CHE-5412): requests of one batch share
-- this id. No foreign key: the id names a group, not a row.
ALTER TABLE decision_request ADD COLUMN IF NOT EXISTS batch_id uuid;
