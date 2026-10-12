ALTER TABLE chat_message
    ADD COLUMN sent_via TEXT
    CHECK (sent_via IN ('interactive_jwt', 'cli_jwt', 'pat', 'task_token', 'cloud_pat', 'channel', 'system'));
