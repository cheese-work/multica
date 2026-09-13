-- Supports ClaimPendingGitHubMergeAnnouncement's bounded SKIP LOCKED scan
-- (reconciliation sweeper), mirroring 178_webhook_delivery_queue_index's
-- queue-claim index shape.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_github_merge_announcement_pending_claim
    ON github_merge_announcement (available_at, created_at)
    WHERE status = 'pending';
