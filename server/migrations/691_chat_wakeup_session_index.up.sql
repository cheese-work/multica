CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_wakeup_session
    ON chat_wakeup (chat_session_id) WHERE status = 'pending';
