-- name: GetGitHubWebhookRevision :one
SELECT * FROM github_webhook_revision WHERE workspace_id = $1 AND id = $2;

-- name: GetGitHubWebhookRevisionForUpdate :one
SELECT * FROM github_webhook_revision WHERE workspace_id = $1 AND id = $2 FOR UPDATE;

-- name: InsertGitHubSnapshotApproval :execrows
INSERT INTO github_snapshot_approval (
    workspace_id, revision_id, approver_user_id, snapshot_hash, object_kind, object_id,
    author_id, body_hash, review_state, editor_type, editor_id, last_edited_at, read_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, sqlc.narg(last_edited_at), $12)
ON CONFLICT (workspace_id, revision_id) DO NOTHING;

-- name: GetGitHubSnapshotApproval :one
SELECT * FROM github_snapshot_approval WHERE workspace_id = $1 AND revision_id = $2;

-- name: ListApprovedGitHubRevisions :many
SELECT workspace_id, id FROM github_webhook_revision
WHERE state = 'deferred' AND reason = 'approved_snapshot'
ORDER BY updated_at LIMIT $1;
