CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_github_merge_announcement_identity
    ON github_merge_announcement (workspace_id, provider, repository_id, pr_number, issue_id, event_kind);
