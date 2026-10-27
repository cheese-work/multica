-- name: InsertGitHubWebhookIntake :execrows
INSERT INTO github_webhook_intake (delivery_guid, event, signature, payload)
VALUES ($1, $2, $3, $4)
ON CONFLICT (delivery_guid) DO NOTHING;

-- name: GetGitHubWebhookIntake :one
SELECT * FROM github_webhook_intake WHERE delivery_guid = $1;

-- name: SetGitHubWebhookIntakeState :exec
UPDATE github_webhook_intake SET state = $2, reason = $3, updated_at = now()
WHERE delivery_guid = $1;

-- name: InsertGitHubWebhookRevision :execrows
INSERT INTO github_webhook_revision (
    workspace_id, installation_id, repository_id, pull_request_id, pr_number,
    object_kind, object_id, body_hash, source_delivery_guid, event, action,
    sender_id, author_id, author_login, author_type, head_sha, review_state,
    object_created_at, object_updated_at, object_submitted_at, state, reason
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)
ON CONFLICT (workspace_id, object_kind, object_id, body_hash) DO NOTHING;
