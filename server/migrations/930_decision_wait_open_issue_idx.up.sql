CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS decision_wait_open_issue_idx
    ON decision_wait (issue_id) WHERE closed_at IS NULL;
