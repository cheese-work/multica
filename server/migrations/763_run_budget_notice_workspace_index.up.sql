-- Supports the workspace-teardown delete of run_budget_notice.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_run_budget_notice_workspace_id
    ON run_budget_notice (workspace_id);
