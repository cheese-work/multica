CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS wakeup_aggregate_reservation_identity_idx ON wakeup_aggregate_reservation(workspace_id, scope_kind, scope_id, rule_key, task_id);
