CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_github_webhook_revision_identity
ON github_webhook_revision (
    workspace_id, installation_id, repository_id, object_kind, object_id,
    event, action, body_hash, sender_id, author_id, review_state,
    object_created_at, object_updated_at, object_submitted_at
) NULLS NOT DISTINCT;
