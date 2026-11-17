CREATE INDEX CONCURRENTLY IF NOT EXISTS weekly_run_guard_allowance_issue_idx
    ON weekly_run_guard_allowance (issue_id, created_at);
