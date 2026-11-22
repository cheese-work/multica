CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS merge_queue_state_pr_uidx ON merge_queue_state (workspace_id, repo_owner, repo_name, pr_number);
