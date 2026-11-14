CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS ci_watch_event_notify_uidx ON ci_watch_event (workspace_id, repo_owner, repo_name, signature_hash, day) WHERE kind = 'notify';
