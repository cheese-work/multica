CREATE INDEX CONCURRENTLY IF NOT EXISTS issue_wakeup_capacity_held_idx ON issue_wakeup(updated_at, id) WHERE capacity_reason IS NOT NULL;
