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

-- name: TakeGitHubWebhookIntake :one
SELECT * FROM github_webhook_intake
WHERE state = 'deferred' AND next_attempt_at <= now()
ORDER BY next_attempt_at, received_at
LIMIT 1 FOR UPDATE SKIP LOCKED;

-- name: ListGitHubWebhookRevisionsForWork :many
SELECT * FROM github_webhook_revision
WHERE installation_id = $1 AND repository_id = $2
  AND object_kind = $3 AND object_id = $4 AND body_hash = $5
ORDER BY workspace_id
FOR UPDATE;

-- name: SetGitHubWebhookRevisionState :exec
UPDATE github_webhook_revision SET state = $5, reason = $6, updated_at = now()
WHERE workspace_id = $1 AND object_kind = $2 AND object_id = $3 AND body_hash = $4;

-- name: ScheduleGitHubWebhookIntakeRetry :exec
UPDATE github_webhook_intake
SET state = 'deferred', reason = $2, updated_at = now(),
    attempts = LEAST(attempts::bigint + 1, 2147483647)::integer,
    next_attempt_at = now() + sqlc.arg(delay_seconds)::bigint * interval '1 second'
WHERE delivery_guid = $1;

-- name: ListDeferredGitHubWebhookIntakes :many
SELECT delivery_guid, event, reason, attempts, received_at, next_attempt_at,
       EXTRACT(EPOCH FROM (now() - received_at))::bigint AS age_seconds
FROM github_webhook_intake WHERE state = 'deferred'
ORDER BY received_at LIMIT $1;

-- name: HasGitHubWebhookDispatch :one
SELECT EXISTS (SELECT 1 FROM github_webhook_dispatch
WHERE workspace_id = $1 AND object_kind = $2 AND object_id = $3 AND body_hash = $4 AND issue_id = $5);

-- name: RecordGitHubWebhookDispatch :execrows
INSERT INTO github_webhook_dispatch (workspace_id, object_kind, object_id, body_hash, issue_id, comment_id, task_id)
SELECT $1, $2, $3, $4, $5, $6, $7
WHERE EXISTS (
    SELECT 1 FROM agent_task_queue task JOIN comment ON comment.id = task.trigger_comment_id
    WHERE task.id = $7 AND task.workspace_id = $1 AND task.issue_id = $5
      AND comment.id = $6 AND comment.workspace_id = $1 AND comment.issue_id = $5
);
