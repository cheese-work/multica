-- CHE-3555 (M17): read is its own stored state, apart from delivered
-- (ping_notified_at) and answered (status).
ALTER TABLE decision_request ADD COLUMN IF NOT EXISTS read_at timestamptz;
