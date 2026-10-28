-- At most one notice per (issue, window, kind): see migration 753.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_one_run_budget_notice_per_window
    ON run_budget_notice (issue_id, window_id, kind);
