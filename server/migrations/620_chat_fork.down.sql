ALTER TABLE chat_session
    DROP COLUMN IF EXISTS fork_message_count,
    DROP COLUMN IF EXISTS fork_last_message_id,
    DROP COLUMN IF EXISTS fork_idempotency_key,
    DROP COLUMN IF EXISTS forked_from_session_id;
