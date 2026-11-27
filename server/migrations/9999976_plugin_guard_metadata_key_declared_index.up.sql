CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_plugin_guard_metadata_key_declared
    ON plugin_guard_metadata_key (workspace_id, metadata_key)
    WHERE enabled;
