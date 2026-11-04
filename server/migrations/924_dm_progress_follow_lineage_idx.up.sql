CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_dm_progress_follow_lineage
    ON dm_progress_follow (lineage_id);
