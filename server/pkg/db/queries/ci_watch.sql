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
