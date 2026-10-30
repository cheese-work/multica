CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_run_capability_grant_issue ON run_capability_grant (issue_id, expires_at) WHERE revoked_at IS NULL;
