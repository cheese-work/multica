-- CHE-755: audit review lists a workspace's exports newest-first. Built
-- CONCURRENTLY and in its own migration file per repo convention (AGENTS.md).
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_provenance_export_log_workspace_created
    ON provenance_export_log (workspace_id, created_at);
