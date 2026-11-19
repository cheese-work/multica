CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS merge_queue_wait_unmet_uidx ON merge_queue_wait (workspace_id, repo_owner, repo_name, pr_number, issue_id) WHERE state = 'unmet';
