CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS governance_budget_reservation_attempt_obligation_uidx
ON governance_budget_reservation (workspace_id, attempt_id, obligation_id);
