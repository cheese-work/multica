CREATE INDEX CONCURRENTLY IF NOT EXISTS github_workflow_run_head_sha_idx ON github_workflow_run (workspace_id, repo_owner, repo_name, head_sha);
