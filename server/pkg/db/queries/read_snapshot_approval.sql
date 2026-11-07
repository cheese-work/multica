-- name: InsertReadSnapshotApproval :execrows
INSERT INTO read_snapshot_approval (workspace_id, issue_id, object_kind, object_id, body_hash, approver_user_id)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT DO NOTHING;

-- name: HasReadSnapshotApproval :one
SELECT EXISTS (
    SELECT 1 FROM read_snapshot_approval
    WHERE workspace_id = $1 AND issue_id = $2 AND object_kind = $3 AND object_id = $4 AND body_hash = $5
);

-- name: ApproveIssueCommentSet :execrows
INSERT INTO read_snapshot_approval (workspace_id, issue_id, object_kind, object_id, body_hash, approver_user_id)
SELECT c.workspace_id, c.issue_id, 'multica_comment', c.id::text, encode(sha256(convert_to(c.content, 'UTF8')), 'hex'), $3
FROM comment c
WHERE c.workspace_id = $1 AND c.issue_id = $2
ON CONFLICT DO NOTHING;
