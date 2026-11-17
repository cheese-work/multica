CREATE INDEX CONCURRENTLY IF NOT EXISTS dm_progress_pending_lineage_created_idx ON dm_progress_pending (lineage_id, created_at);
