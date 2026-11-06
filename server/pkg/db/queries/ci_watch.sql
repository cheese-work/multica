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
