CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS governance_budget_reservation_receipt_uidx
ON governance_budget_reservation (workspace_id, settlement_receipt_id)
WHERE settlement_receipt_id IS NOT NULL;
