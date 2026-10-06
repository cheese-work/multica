CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS wakeup_aggregate_budget_identity_idx ON wakeup_aggregate_budget(workspace_id, scope_kind, scope_id, rule_key);
