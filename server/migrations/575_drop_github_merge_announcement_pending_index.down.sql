CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_github_merge_announcement_pending_claim
    ON github_merge_announcement (available_at, created_at)
    WHERE status = 'pending';
