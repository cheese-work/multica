CREATE INDEX CONCURRENTLY IF NOT EXISTS governance_budget_reservation_open_window_idx
ON governance_budget_reservation (workspace_id, window_start)
WHERE state = 'reserved';
