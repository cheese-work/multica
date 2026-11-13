-- At most one refusal per (issue, window): see migration 902.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_one_run_budget_refusal_per_window
    ON run_budget_refusal (issue_id, window_id);
