CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS issue_wakeup_definition_identity_idx ON issue_wakeup_definition(workspace_id, scope_kind, scope_id, rule_key);
