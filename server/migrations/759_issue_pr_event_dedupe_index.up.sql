CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_pr_event_dedupe
    ON issue_pr_event (issue_id, pull_request_id, dedupe_key);
