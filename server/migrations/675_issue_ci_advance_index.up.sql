CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_ci_advance_issue_head ON issue_ci_advance (issue_id, head_sha);
