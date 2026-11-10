CREATE INDEX CONCURRENTLY IF NOT EXISTS wakeup_scoped_event_delayed_idx ON wakeup_scoped_event(captured_at, id) WHERE handled_at IS NULL AND retry_at IS NOT NULL;
