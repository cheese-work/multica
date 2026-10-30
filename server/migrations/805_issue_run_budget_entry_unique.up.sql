-- One entry per (issue, kind, event_key): a replayed event is a no-op.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_one_issue_run_budget_entry
    ON issue_run_budget_entry (issue_id, kind, event_key);
