CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_github_webhook_revision_key
ON github_webhook_revision (workspace_id, object_kind, object_id, body_hash);
