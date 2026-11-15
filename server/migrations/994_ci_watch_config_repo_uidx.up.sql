CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS ci_watch_config_repo_uidx ON ci_watch_config (workspace_id, repo_owner, repo_name);
