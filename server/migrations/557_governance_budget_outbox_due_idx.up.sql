CREATE INDEX CONCURRENTLY IF NOT EXISTS governance_budget_outbox_due_idx
ON governance_budget_outbox (workspace_id, state, created_at, event_id);
