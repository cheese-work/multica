CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_dm_progress_data_incident_follow_kind
    ON dm_progress_data_incident (follow_id, kind);
