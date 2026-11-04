-- Supports the workspace-teardown delete of issue_run_budget_entry.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_run_budget_entry_workspace_id
    ON issue_run_budget_entry (workspace_id);
