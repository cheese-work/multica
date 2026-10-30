-- name: ListActiveRunCapabilityGrants :many
SELECT * FROM run_capability_grant
WHERE workspace_id = $1 AND issue_id = $2 AND revoked_at IS NULL AND expires_at > now()
ORDER BY created_at;

-- name: UpsertProtectedPathPolicy :one
INSERT INTO protected_path_policy (workspace_id, name, patterns, updated_by_member_id)
VALUES ($1, $2, $3, $4)
ON CONFLICT (workspace_id, name) DO UPDATE
SET patterns = EXCLUDED.patterns, updated_by_member_id = EXCLUDED.updated_by_member_id, updated_at = clock_timestamp()
RETURNING *;

-- name: GetProtectedPathPolicy :one
SELECT * FROM protected_path_policy WHERE workspace_id = $1 AND name = $2;
