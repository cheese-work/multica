CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_governance_attempt_workspace_created
    ON governance_attempt (workspace_id, created_at DESC, id DESC);
