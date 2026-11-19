CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS merge_queue_epoch_key_uidx ON merge_queue_epoch (workspace_id, repo_owner, repo_name, cause_key, epoch_no);
