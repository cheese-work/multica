SET LOCAL lock_timeout = '2s';

ALTER TABLE chat_message
    ADD COLUMN sent_via TEXT;

ALTER TABLE chat_message
    ADD CONSTRAINT chat_message_sent_via_check
    CHECK (sent_via IN ('interactive_jwt', 'cli_jwt', 'pat', 'task_token', 'cloud_pat', 'channel', 'system')) NOT VALID;
