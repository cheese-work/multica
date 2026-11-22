CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_handoff_operation_created
    ON issue_handoff_operation (created_at);
