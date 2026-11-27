CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_plugin_guard_metadata_key_installation
    ON plugin_guard_metadata_key (installation_id, metadata_key);
