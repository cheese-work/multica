CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS github_workflow_job_identity_uidx ON github_workflow_job (workspace_id, repo_owner, repo_name, job_id);
