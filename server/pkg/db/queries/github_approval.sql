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

-- name: HasAdmittedGitHubRevision :one
-- An admitted intake revision (dispatched, or projected while gate G is closed)
-- of this exact object read, on a PR linked to the issue.
SELECT EXISTS (
    SELECT 1 FROM github_webhook_revision r
    JOIN issue_pull_request l ON l.pull_request_id = r.pull_request_id AND l.issue_id = $2
    WHERE r.workspace_id = $1 AND r.object_kind = $3 AND r.object_id = $4
      AND r.body_hash = $5 AND r.author_id = $6 AND r.review_state = $7
      AND r.state IN ('dispatched', 'projected')
);

-- name: HasGitHubSnapshotApproval :one
-- An exact-snapshot approval (the hash covers editor and last edit time) for a
-- revision on a PR linked to the issue.
SELECT EXISTS (
    SELECT 1 FROM github_snapshot_approval a
    JOIN github_webhook_revision r ON r.workspace_id = a.workspace_id AND r.id = a.revision_id
    JOIN issue_pull_request l ON l.pull_request_id = r.pull_request_id AND l.issue_id = $2
    WHERE a.workspace_id = $1 AND a.snapshot_hash = $3
);
