CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_github_webhook_dispatch_revision_key ON github_webhook_dispatch (workspace_id, revision_id, issue_id);
