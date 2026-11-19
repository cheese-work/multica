CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS merge_queue_association_active_uidx ON merge_queue_association (workspace_id, repo_owner, repo_name, pr_number, cause_key) WHERE ended_at IS NULL;
