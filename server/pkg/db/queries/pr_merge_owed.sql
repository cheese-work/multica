-- The pr_merge_owed system rule (CHE-5243): one row per issue whose PR merge
-- still owes a completion decision. Its (id, revision) is the generation that
-- fences completion: only the attempt that read a generation may delete or
-- reschedule it, so a newer merge event is never lost to an older attempt.

-- name: UpsertPRMergeOwed :one
-- Record the debt, due in two minutes. A conflict keeps the row's id and bumps
-- its revision, and starts a fresh retry history.
INSERT INTO issue_wakeup(id, workspace_id, issue_id, instruction, kind, mode, event_types, timezone,
                         condition_state, enabled, system_rule, next_fire_at, created_at, updated_at)
VALUES (sqlc.arg('id')::uuid, sqlc.arg('workspace_id')::uuid, sqlc.arg('issue_id')::uuid, '', 'at', 'once', '{}', 'UTC',
        '', true, 'pr_merge_owed', now() + interval '2 minutes', now(), now())
ON CONFLICT (issue_id, system_rule) WHERE system_rule IS NOT NULL DO UPDATE
SET revision = issue_wakeup.revision + 1,
    next_fire_at = EXCLUDED.next_fire_at,
    fire_count = 0,
    last_error = NULL,
    updated_at = clock_timestamp()
RETURNING id, revision;

-- name: GetPRMergeOwedGeneration :one
-- The generation an attempt acts on. Read in its own statement before any
-- decision fact; no row means no generation. fire_count is the failed attempts
-- of this generation, which sets the retry backoff.
SELECT id, revision, fire_count FROM issue_wakeup
WHERE issue_id = sqlc.arg('issue_id')::uuid AND system_rule = 'pr_merge_owed';

-- name: DeletePRMergeOwedGeneration :execrows
-- READ COMMITTED re-checks this predicate after a lock wait, so a delete that
-- waited behind a revision bump matches nothing.
DELETE FROM issue_wakeup
WHERE id = sqlc.arg('id')::uuid AND system_rule = 'pr_merge_owed' AND revision = sqlc.arg('revision')::bigint;

-- name: ReschedulePRMergeOwedGeneration :execrows
-- Changes the retry state only, never the revision.
UPDATE issue_wakeup
SET next_fire_at = sqlc.arg('next_fire_at')::timestamptz,
    last_error = sqlc.arg('last_error')::text,
    fire_count = fire_count + 1,
    updated_at = clock_timestamp()
WHERE id = sqlc.arg('id')::uuid AND system_rule = 'pr_merge_owed' AND revision = sqlc.arg('revision')::bigint;
