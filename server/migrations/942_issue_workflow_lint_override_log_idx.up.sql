CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_workflow_lint_override_workflow ON issue_workflow_lint_override (workflow_id, version DESC);
