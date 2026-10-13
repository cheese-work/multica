CREATE INDEX CONCURRENTLY IF NOT EXISTS issue_wakeup_definition_sweep_idx ON issue_wakeup_definition(updated_at, rule_key) WHERE NOT sweep_done OR sweep_revision <> revision;
