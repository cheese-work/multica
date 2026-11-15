-- CHE-4273: CI failure watcher storage. Nothing calls these while the
-- ci_watcher feature flag is off.

-- name: DeleteExpiredCIFailureEvents :execrows
DELETE FROM ci_failure_event WHERE created_at < $1;

-- name: InsertCIFailureEvent :execrows
-- CHE-4274: idempotent on (workspace, repo, run id, attempt); a redelivered
-- run changes nothing.
INSERT INTO ci_failure_event (
    workspace_id, repo_owner, repo_name, run_id, run_attempt, workflow_name, event,
    head_branch, head_sha, conclusion, signature_hash, evidence, partial_reason,
    failed_job_count, excerpts
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15
)
ON CONFLICT (workspace_id, repo_owner, repo_name, run_id, run_attempt) DO NOTHING;

-- name: CreateSystem1CIObservation :execrows
-- One rule row per CI run attempt (subject_id is derived from the run key). A
-- redelivery, or a retry after a failed write, inserts nothing. FOR KEY SHARE
-- as in CreateSystem1Observation: no row for a workspace being deleted.
INSERT INTO system1_observation (workspace_id, kind, subject_id, class, decision_source, inputs, predicted_key, predicted_confidence, predicted_precedents, predicted_at, choice_key)
SELECT workspace.id, 'ci_event', sqlc.arg('subject_id')::uuid, sqlc.arg('class')::text, 'rule', sqlc.arg('inputs')::jsonb, sqlc.narg('predicted_key')::text, sqlc.narg('predicted_confidence')::real, sqlc.narg('predicted_precedents')::int, CASE WHEN sqlc.narg('predicted_key')::text IS NOT NULL THEN clock_timestamp() END, sqlc.arg('choice_key')::text
FROM workspace WHERE workspace.id = sqlc.arg('workspace_id')
FOR KEY SHARE OF workspace
ON CONFLICT (workspace_id, subject_id) WHERE kind = 'ci_event' DO NOTHING;

-- name: LockWorkspaceForCIWrite :one
-- Takes a KEY SHARE lock on the workspace row until the transaction ends: a
-- workspace delete waits for the CI writes, and a workspace already deleted
-- returns no row, so the caller can write nothing.
SELECT id FROM workspace WHERE id = $1 FOR KEY SHARE;

-- name: GetCIWatchConfigForRepo :one
-- The per-repo routing config; no row means the repo is not watched.
SELECT mode, branches, events, workflows_include, workflows_exclude
FROM ci_watch_config
WHERE workspace_id = $1 AND lower(repo_owner) = lower($2) AND lower(repo_name) = lower($3);

-- name: CIWatchNewerRuns :one
-- Supersession from the run mirror, no API call: is there a later run (higher
-- run id) of the same workflow and branch, and did one of them succeed.
SELECT (count(*) > 0)::bool AS newer_exists,
       COALESCE(bool_or(conclusion = 'success'), false)::bool AS newer_succeeded
FROM github_workflow_run
WHERE workspace_id = $1 AND repo_owner = lower($2) AND repo_name = lower($3)
  AND workflow_path = $4 AND head_branch = $5 AND run_id > $6;
