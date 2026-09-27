CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_governance_receipt_workspace_comment_created
    ON governance_receipt (workspace_id, comment_id, created_at DESC, id DESC);
