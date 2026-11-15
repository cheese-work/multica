CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS ci_watch_action_key_uidx ON ci_watch_action (workspace_id, repo_owner, repo_name, original_run_id, action_kind);
