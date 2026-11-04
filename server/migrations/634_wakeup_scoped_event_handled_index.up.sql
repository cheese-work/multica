CREATE INDEX CONCURRENTLY IF NOT EXISTS wakeup_scoped_event_handled_idx ON wakeup_scoped_event(handled_at) WHERE handled_at IS NOT NULL;
