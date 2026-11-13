SET LOCAL lock_timeout = '2s';

ALTER TABLE chat_message DROP COLUMN IF EXISTS client_message_id;
