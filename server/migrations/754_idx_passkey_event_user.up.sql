CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_passkey_event_user ON passkey_event (user_id, created_at);
