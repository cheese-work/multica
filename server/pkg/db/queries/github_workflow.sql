-- =====================
-- GitHub workflow runs and jobs (CHE-2147)
--
-- Read side of the webhook-fed run/job mirror behind the `github_run` wakeup
-- condition. Every query is scoped by workspace_id; the repo owner and name are
-- stored lowercased, so callers pass them lowercased.
-- =====================

-- name: GetGitHubWorkflowRun :one
SELECT * FROM github_workflow_run
WHERE workspace_id = $1 AND repo_owner = $2 AND repo_name = $3 AND run_id = $4;

-- name: ListGitHubWorkflowRunsByHeadSHA :many
SELECT * FROM github_workflow_run
WHERE workspace_id = $1 AND repo_owner = $2 AND repo_name = $3 AND head_sha = $4
ORDER BY run_id;

-- name: ListGitHubWorkflowJobsByRunAttempt :many
SELECT * FROM github_workflow_job
WHERE workspace_id = $1 AND repo_owner = $2 AND repo_name = $3
  AND run_id = $4 AND run_attempt = $5
ORDER BY job_id;
