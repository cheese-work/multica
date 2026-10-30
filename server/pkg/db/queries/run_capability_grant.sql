-- name: CreateRunCapabilityGrant :one
INSERT INTO run_capability_grant (workspace_id, issue_id, scopes, granted_by_member_id, expires_at, reason, pinned_base_sha, pinned_head_sha)
VALUES ($1, $2, $3, $4, $5, $6, sqlc.narg('pinned_base_sha'), sqlc.narg('pinned_head_sha'))
RETURNING *;
