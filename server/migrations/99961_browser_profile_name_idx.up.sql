CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS browser_profile_workspace_name_uidx
    ON browser_profile (workspace_id, name);
