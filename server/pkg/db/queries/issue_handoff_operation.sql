-- name: InsertHandoffOperation :one
-- No row back means the key already has a receipt.
INSERT INTO issue_handoff_operation (workspace_id, issue_id, operation_key, actor_type, actor_id, request_hash)
VALUES (@workspace_id, @issue_id, @operation_key, @actor_type, sqlc.narg(actor_id), @request_hash)
ON CONFLICT (issue_id, operation_key) DO NOTHING
RETURNING *;

-- name: GetHandoffOperation :one
SELECT * FROM issue_handoff_operation WHERE issue_id = @issue_id AND operation_key = @operation_key;

-- name: RecordHandoffOperationWrites :exec
-- Same transaction as the insert: what the move wrote, and the replayable response.
UPDATE issue_handoff_operation
SET response = sqlc.narg(response),
    status_history_id = sqlc.narg(status_history_id),
    created_comment_id = sqlc.narg(created_comment_id),
    dispatch_id = sqlc.narg(dispatch_id)
WHERE issue_id = @issue_id AND operation_key = @operation_key;

-- name: StoreHandoffOperationResponse :exec
-- After the commit: the full response the first request sent replaces the one
-- the transaction stored.
UPDATE issue_handoff_operation SET response = @response
WHERE issue_id = @issue_id AND operation_key = @operation_key AND response IS NOT NULL;

-- name: DeleteHandoffOperationsBefore :execrows
DELETE FROM issue_handoff_operation WHERE created_at < @cutoff;
