CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS github_workflow_run_identity_uidx ON github_workflow_run (workspace_id, repo_owner, repo_name, run_id);
