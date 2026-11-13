CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_session_chain_root
    ON chat_session (chain_root_session_id);
