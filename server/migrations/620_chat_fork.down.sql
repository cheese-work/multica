ALTER TABLE chat_session
    DROP COLUMN IF EXISTS fork_idempotency_key,
    DROP COLUMN IF EXISTS forked_from_session_id;
