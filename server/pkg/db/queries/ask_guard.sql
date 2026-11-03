-- name: InsertAskGuardEvent :one
-- A conflict on the operation key returns no row; the caller reads the
-- existing event with GetAskGuardEventByOperation.
INSERT INTO ask_guard_event (id, workspace_id, issue_id, task_id, operation_kind, payload_sha256, mode)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (task_id, operation_kind, payload_sha256, mode) DO NOTHING
RETURNING *;

-- name: GetAskGuardEventByOperation :one
SELECT * FROM ask_guard_event
WHERE task_id = $1 AND operation_kind = $2 AND payload_sha256 = $3 AND mode = $4;
