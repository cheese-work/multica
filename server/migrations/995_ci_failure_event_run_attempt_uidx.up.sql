CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS ci_failure_event_run_attempt_uidx ON ci_failure_event (workspace_id, repo_owner, repo_name, run_id, run_attempt);
