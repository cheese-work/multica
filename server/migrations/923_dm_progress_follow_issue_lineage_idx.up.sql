CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_dm_progress_follow_issue_lineage
    ON dm_progress_follow (issue_id, lineage_id);
