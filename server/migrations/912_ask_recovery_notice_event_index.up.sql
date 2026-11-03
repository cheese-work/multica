CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_ask_recovery_notice_event
    ON ask_recovery_notice (event_id);
