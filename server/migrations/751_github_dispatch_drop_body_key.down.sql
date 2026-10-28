CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_github_webhook_dispatch_key ON github_webhook_dispatch (workspace_id, object_kind, object_id, body_hash, issue_id);
