-- name: InsertTaskElevation :exec
INSERT INTO task_elevation (task_id, workspace_id, issue_id, grant_ids, scopes)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (task_id) DO NOTHING;

-- name: GetTaskElevation :one
SELECT * FROM task_elevation WHERE task_id = $1;

-- name: LockTaskElevation :one
SELECT * FROM task_elevation WHERE task_id = $1 FOR UPDATE;

-- name: CountLiveGrantsByID :one
SELECT count(*) FROM run_capability_grant
WHERE id = ANY($1::uuid[]) AND revoked_at IS NULL AND expires_at > clock_timestamp();

-- name: CreateElevatedOp :one
INSERT INTO elevated_op (workspace_id, task_id, epoch, scope, input_hash, lease_until)
VALUES ($1, $2, $3, $4, $5, clock_timestamp() + interval '10 minutes')
RETURNING *;

-- name: CompleteElevatedOp :one
UPDATE elevated_op SET state = $3, updated_at = clock_timestamp()
WHERE op_id = $1 AND task_id = $2 AND state IN ('authorized', 'unknown')
RETURNING *;

-- name: FenceLapsedTaskElevations :many
UPDATE task_elevation te SET state = 'fencing', epoch = te.epoch + 1, updated_at = clock_timestamp()
WHERE te.state = 'elevated' AND (
    SELECT count(*) FROM run_capability_grant g
    WHERE g.id = ANY(te.grant_ids) AND g.revoked_at IS NULL AND g.expires_at > clock_timestamp()
) < cardinality(te.grant_ids)
RETURNING te.task_id;

-- name: MarkLapsedElevatedOpsUnknown :many
UPDATE elevated_op SET state = 'unknown', updated_at = clock_timestamp()
WHERE state = 'authorized' AND lease_until <= clock_timestamp()
RETURNING op_id, task_id;

-- name: FinishDrainedTaskFences :many
UPDATE task_elevation te SET state = 'restricted_final', updated_at = clock_timestamp()
WHERE te.state = 'fencing'
  AND NOT EXISTS (SELECT 1 FROM elevated_op o WHERE o.task_id = te.task_id AND o.state IN ('authorized', 'unknown'))
  AND (te.launch_scopes_ended_at IS NOT NULL
       OR NOT EXISTS (SELECT 1 FROM unnest(te.scopes) s WHERE s = 'docker' OR s LIKE 'device:%'))
RETURNING te.task_id;

-- name: FenceTaintedTaskElevations :many
-- A task whose taint grew after its elevation was recorded stops being
-- elevated. task_id narrows it to one task (the claim); NULL sweeps them all.
UPDATE task_elevation te SET state = 'fencing', epoch = te.epoch + 1, updated_at = clock_timestamp()
FROM agent_task_queue t
WHERE t.id = te.task_id AND te.state = 'elevated' AND cardinality(t.taint_ids) > 0
  AND (sqlc.narg('task_id')::uuid IS NULL OR te.task_id = sqlc.narg('task_id')::uuid)
RETURNING te.task_id;

-- name: LockTaskTaintIDs :one
-- Share lock: a taint writer updates the task row, so it waits for an
-- authorization in flight, and an authorization that starts after it sees it.
SELECT taint_ids FROM agent_task_queue WHERE id = $1 FOR SHARE;
