CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_wakeup_pending ON chat_wakeup (fire_at) WHERE status = 'pending';
