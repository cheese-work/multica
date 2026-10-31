CREATE INDEX CONCURRENTLY IF NOT EXISTS wakeup_scoped_event_issue_idx ON wakeup_scoped_event(issue_id, captured_at, id) WHERE handled_at IS NULL;
