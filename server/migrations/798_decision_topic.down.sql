SET LOCAL lock_timeout = '2s';

ALTER TABLE chat_session DROP COLUMN IF EXISTS decision_topic_id;
