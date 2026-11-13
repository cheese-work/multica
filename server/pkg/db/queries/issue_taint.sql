-- name: LockIssueForTaint :exec
-- Share lock: a taint writer holds the issue row (issue_taint_note), so an
-- authorization waits for it and one that starts after sees the new taint.
SELECT 1 FROM issue WHERE id = $1 FOR SHARE;

-- name: LockIssueForTaintClear :exec
SELECT 1 FROM issue WHERE id = $1 FOR NO KEY UPDATE;

-- name: GetIssueTaintIDs :one
SELECT COALESCE((SELECT taint_ids FROM issue_taint WHERE issue_id = $1), '{}')::text[] AS taint_ids;

-- name: ClearIssueTaintIDs :exec
UPDATE issue_taint SET updated_at = clock_timestamp(),
    taint_ids = ARRAY(SELECT t FROM unnest(taint_ids) AS t WHERE t <> ALL (sqlc.arg('cleared')::text[]))
WHERE issue_id = sqlc.arg('issue_id');

-- name: InsertIssueTaintClear :one
INSERT INTO issue_taint_clear (workspace_id, issue_id, taint_ids, cleared_by_member_id)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: NoteIssueTaint :exec
SELECT issue_taint_note(@issue_id::uuid, @taint::text[]);

-- name: NoteIssueRunTaint :exec
-- A tainted writer's whole taint, kept apart from the issue's own taint so a
-- member's clear, before or after the write, never drops it. Written in the
-- writer's own transaction.
INSERT INTO issue_run_taint (issue_id, workspace_id, taint_ids)
SELECT i.id, i.workspace_id, array_agg(DISTINCT t ORDER BY t)
FROM issue i, unnest(@taint::text[]) AS t
WHERE i.id = @issue_id::uuid
GROUP BY i.id, i.workspace_id
ON CONFLICT (issue_id) DO UPDATE SET
    taint_ids = (SELECT array_agg(DISTINCT u ORDER BY u) FROM unnest(issue_run_taint.taint_ids || EXCLUDED.taint_ids) AS u);

-- name: GetIssueRunTaint :one
SELECT COALESCE((SELECT taint_ids FROM issue_run_taint WHERE issue_id = $1), '{}')::text[] AS taint_ids;

-- name: TaintBudgetRecoveryRuns :exec
-- A requester's taint reaches a recovery run that already exists, whether it is
-- still queued, waiting for a local directory, or already started (the task
-- trigger unions it with what the run has, and a started run reads its taint on
-- every call).
UPDATE agent_task_queue SET taint_ids = @taint::text[]
WHERE issue_id = @issue_id::uuid AND task_kind = 'budget_recovery'
  AND status IN ('queued', 'dispatched', 'waiting_local_directory', 'running');
