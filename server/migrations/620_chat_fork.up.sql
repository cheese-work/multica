ALTER TABLE chat_session
    ADD COLUMN forked_from_session_id uuid,
    ADD COLUMN fork_idempotency_key text;
