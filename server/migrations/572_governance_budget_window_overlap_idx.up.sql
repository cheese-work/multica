CREATE INDEX CONCURRENTLY IF NOT EXISTS governance_budget_window_overlap_idx
ON governance_budget_window (workspace_id, window_end, window_start);
