SET LOCAL lock_timeout = '2s';

-- Idempotent DM send (CHE-4146, plan r4 B4). The client mints one id per send attempt and
-- reuses it on retry; NULL for every message written before this column or by a non-send path.
ALTER TABLE chat_message ADD COLUMN IF NOT EXISTS client_message_id text;
