-- Case listing and reconciliation start with a workspace and use a bounded
-- newest-first keyset page.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_governance_case_workspace_created
    ON governance_case (workspace_id, created_at DESC, id DESC);
