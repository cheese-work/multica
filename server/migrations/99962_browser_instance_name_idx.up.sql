CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS browser_instance_workspace_name_uidx
    ON browser_instance (workspace_id, name);
