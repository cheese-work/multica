-- Trigram index for DM title search (GET /api/chat/search). Single statement
-- because CREATE INDEX CONCURRENTLY cannot run inside a transaction.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_session_title_trgm
    ON chat_session USING gin (LOWER(title) gin_trgm_ops);
