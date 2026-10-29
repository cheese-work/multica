CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_pr_event_latest
    ON issue_pr_event (issue_id, pull_request_id, head_sha, observed_at DESC);
