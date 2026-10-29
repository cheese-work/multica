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

-- =====================
-- Guarded writes (CHE-2147 D1b)
--
-- Called in this order inside one transaction per workspace by the ingest
-- store (internal/integrations/ghrun): ensure stub, lock the run row, then the
-- attempt / status / job / gate-generation steps. The lock serializes every
-- event for one run.
-- =====================

-- name: EnsureGitHubWorkflowRunStub :exec
INSERT INTO github_workflow_run (
    workspace_id, installation_id, repo_owner, repo_name, run_id, run_attempt, head_sha
) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (workspace_id, repo_owner, repo_name, run_id) DO NOTHING;

-- name: LockGitHubWorkflowRun :one
SELECT * FROM github_workflow_run
WHERE workspace_id = $1 AND repo_owner = $2 AND repo_name = $3 AND run_id = $4
FOR UPDATE;

-- A run event with a higher attempt replaces every run column.
-- name: ReplaceGitHubWorkflowRunAttempt :exec
UPDATE github_workflow_run SET
    installation_id = $5,
    run_attempt = $6,
    status_rank = $7,
    status = $8,
    conclusion = $9,
    gh_updated_at = sqlc.narg('gh_updated_at'),
    gate_generation = 0,
    workflow_path = $10,
    workflow_name = $11,
    display_title = $12,
    head_sha = $13,
    head_branch = $14,
    event = $15,
    html_url = $16,
    updated_at = now()
WHERE workspace_id = $1 AND repo_owner = $2 AND repo_name = $3 AND run_id = $4;

-- A job event with a higher attempt raises the pointer and resets the status
-- fields to a stub. Per-run metadata (a re-run does not change it) is kept.
-- name: RaiseGitHubWorkflowRunAttemptFromJob :exec
UPDATE github_workflow_run SET
    run_attempt = $5,
    status_rank = -1,
    status = '',
    conclusion = '',
    gh_updated_at = NULL,
    gate_generation = 0,
    updated_at = now()
WHERE workspace_id = $1 AND repo_owner = $2 AND repo_name = $3 AND run_id = $4;

-- A run event for the current attempt. Empty metadata columns are filled; the
-- status fields move only when the caller (holding the row lock) decided the
-- event is not stale.
-- name: AdvanceGitHubWorkflowRun :exec
UPDATE github_workflow_run SET
    workflow_path = CASE WHEN workflow_path = '' THEN $5 ELSE workflow_path END,
    workflow_name = CASE WHEN workflow_name = '' THEN $6 ELSE workflow_name END,
    display_title = CASE WHEN display_title = '' THEN $7 ELSE display_title END,
    head_sha = CASE WHEN head_sha = '' THEN $8 ELSE head_sha END,
    head_branch = CASE WHEN head_branch = '' THEN $9 ELSE head_branch END,
    event = CASE WHEN event = '' THEN $10 ELSE event END,
    html_url = CASE WHEN html_url = '' THEN $11 ELSE html_url END,
    status_rank = CASE WHEN sqlc.arg('advance_status')::boolean THEN $12 ELSE status_rank END,
    status = CASE WHEN sqlc.arg('advance_status')::boolean THEN $13 ELSE status END,
    conclusion = CASE WHEN sqlc.arg('advance_status')::boolean THEN $14 ELSE conclusion END,
    gh_updated_at = CASE WHEN sqlc.arg('advance_status')::boolean THEN sqlc.narg('gh_updated_at') ELSE gh_updated_at END,
    updated_at = now()
WHERE workspace_id = $1 AND repo_owner = $2 AND repo_name = $3 AND run_id = $4;

-- Rows affected 1 means a new job row: for phase 1 that is a newly opened gate.
-- name: InsertGitHubWorkflowJob :execrows
INSERT INTO github_workflow_job (
    workspace_id, repo_owner, repo_name, run_id, run_attempt, job_id, job_name, phase
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (workspace_id, repo_owner, repo_name, job_id) DO NOTHING;

-- The phase only rises: waiting 1, in_progress 2, completed 3.
-- name: AdvanceGitHubWorkflowJobPhase :execrows
UPDATE github_workflow_job SET
    phase = $5,
    job_name = CASE WHEN sqlc.arg('job_name')::text = '' THEN job_name ELSE sqlc.arg('job_name')::text END,
    updated_at = now()
WHERE workspace_id = $1 AND repo_owner = $2 AND repo_name = $3 AND job_id = $4
  AND phase < $5;

-- name: IncrementGitHubWorkflowRunGateGeneration :exec
UPDATE github_workflow_run SET
    gate_generation = gate_generation + 1,
    updated_at = now()
WHERE workspace_id = $1 AND repo_owner = $2 AND repo_name = $3 AND run_id = $4
  AND run_attempt = $5;

-- =====================
-- Retention (CHE-2147 D2)
--
-- The mirror is a 30-day cache of observed events, not history. Both deletes
-- are global (no workspace filter) and run on the (updated_at) indexes.
-- =====================

-- name: DeleteExpiredGitHubWorkflowRuns :execrows
DELETE FROM github_workflow_run WHERE updated_at < $1;

-- name: DeleteExpiredGitHubWorkflowJobs :execrows
DELETE FROM github_workflow_job WHERE updated_at < $1;
