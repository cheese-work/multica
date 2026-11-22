CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS merge_queue_pr_close_uidx ON merge_queue_pr_close (workspace_id, repo_owner, repo_name, pr_number, closed_at);
