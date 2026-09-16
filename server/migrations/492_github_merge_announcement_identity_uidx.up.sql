-- Rollout guard (CHE-374/CHE-379): the merge-announcement writer must not be
-- enabled before this uniqueness index exists — see the handler's use of
-- ON CONFLICT DO NOTHING against this exact index, which is what makes
-- concurrent workers and duplicate/redelivered merge events converge on one
-- row instead of one comment per delivery.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_github_merge_announcement_identity
    ON github_merge_announcement (workspace_id, provider, repository_id, pr_number, issue_id, event_kind);
