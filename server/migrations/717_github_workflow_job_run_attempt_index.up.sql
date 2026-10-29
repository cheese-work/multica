CREATE INDEX CONCURRENTLY IF NOT EXISTS github_workflow_job_run_attempt_idx ON github_workflow_job (workspace_id, repo_owner, repo_name, run_id, run_attempt);
