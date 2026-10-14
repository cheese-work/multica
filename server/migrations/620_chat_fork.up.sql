ALTER TABLE chat_session
    ADD COLUMN forked_from_session_id uuid,
    ADD COLUMN fork_idempotency_key text,
    ADD COLUMN fork_last_message_id uuid,
    ADD COLUMN fork_message_count integer;
