CREATE INDEX CONCURRENTLY IF NOT EXISTS governance_budget_reservation_root_window_idx
ON governance_budget_reservation (workspace_id, budget_root_id, window_start, reservation_id);
