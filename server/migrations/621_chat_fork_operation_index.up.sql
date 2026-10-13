CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS chat_session_fork_operation_idx
    ON chat_session (forked_from_session_id, fork_idempotency_key)
    WHERE forked_from_session_id IS NOT NULL;
