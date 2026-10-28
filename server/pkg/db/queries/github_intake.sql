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
ON CONFLICT DO NOTHING;

-- name: TakeGitHubWebhookIntake :one
SELECT * FROM github_webhook_intake
WHERE state = 'deferred' AND next_attempt_at <= now()
ORDER BY next_attempt_at, received_at
LIMIT 1 FOR UPDATE SKIP LOCKED;

-- name: ListGitHubWebhookRevisionsForWork :many
SELECT * FROM github_webhook_revision
WHERE installation_id = $1 AND repository_id = $2
  AND object_kind = $3 AND object_id = $4 AND body_hash = $5
  AND event = $6 AND action = $7 AND sender_id = $8 AND author_id = $9 AND review_state = $10
  AND object_created_at IS NOT DISTINCT FROM sqlc.narg(object_created_at)::timestamptz
  AND object_updated_at IS NOT DISTINCT FROM sqlc.narg(object_updated_at)::timestamptz
  AND object_submitted_at IS NOT DISTINCT FROM sqlc.narg(object_submitted_at)::timestamptz
ORDER BY workspace_id
FOR UPDATE;

-- name: SupersedePendingGitHubWebhookRevisions :exec
WITH revisions AS MATERIALIZED (
    SELECT scoped.* FROM github_webhook_revision scoped
    WHERE scoped.installation_id = $1 AND scoped.repository_id = $2
      AND scoped.object_kind = $3 AND scoped.object_id = $4
    ORDER BY scoped.workspace_id, scoped.source_delivery_guid
    FOR UPDATE
)
UPDATE github_webhook_revision revision
SET state = 'denied', reason = 'superseded', updated_at = now()
FROM revisions pending
WHERE revision.workspace_id = pending.workspace_id
  AND revision.installation_id = pending.installation_id
  AND revision.repository_id = pending.repository_id
  AND revision.object_kind = pending.object_kind AND revision.object_id = pending.object_id
  AND revision.source_delivery_guid = pending.source_delivery_guid
  AND revision.state IN ('deferred', 'quarantined', 'unsigned', 'projected')
  AND EXISTS (
    SELECT 1 FROM revisions newer
    WHERE newer.workspace_id = pending.workspace_id
      AND newer.author_id = pending.author_id AND newer.sender_id = newer.author_id
      AND newer.author_type = 'User' AND newer.action = 'edited' AND newer.state <> 'denied'
      AND (
        (pending.object_kind = 'review' AND (
          pending.action = 'submitted'
          OR (newer.created_at, newer.source_delivery_guid) > (pending.created_at, pending.source_delivery_guid)
        ))
        OR (pending.object_kind <> 'review' AND newer.object_updated_at IS NOT NULL AND (
          newer.object_updated_at > COALESCE(pending.object_updated_at, pending.object_created_at)
          OR (newer.object_updated_at = COALESCE(pending.object_updated_at, pending.object_created_at) AND (
            pending.action = 'created'
            OR (newer.created_at, newer.source_delivery_guid) > (pending.created_at, pending.source_delivery_guid)
          ))
        ))
      )
  );

-- name: SetGitHubWebhookRevisionState :exec
UPDATE github_webhook_revision SET state = $3, reason = $4, updated_at = now()
WHERE workspace_id = $1 AND id = $2;

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

-- name: GetGitHubWebhookDispatch :one
SELECT * FROM github_webhook_dispatch WHERE workspace_id = $1 AND revision_id = $2 AND issue_id = $3;

-- name: HasGitHubWebhookRevisionDispatch :one
SELECT EXISTS (SELECT 1 FROM github_webhook_dispatch
WHERE workspace_id = $1 AND revision_id = $2 AND state = 'dispatched');

-- name: RecordGitHubWebhookDispatch :execrows
INSERT INTO github_webhook_dispatch (workspace_id, object_kind, object_id, body_hash, issue_id, comment_id, task_id, revision_id, state, reason)
SELECT sqlc.arg(workspace_id), sqlc.arg(object_kind), sqlc.arg(object_id), sqlc.arg(body_hash), sqlc.arg(issue_id),
    sqlc.narg(comment_id), sqlc.narg(task_id), sqlc.arg(revision_id), sqlc.arg(state), sqlc.arg(reason)
WHERE EXISTS (SELECT 1 FROM github_webhook_revision WHERE id=sqlc.arg(revision_id) AND workspace_id=sqlc.arg(workspace_id))
AND ((sqlc.arg(state) = 'denied' AND sqlc.narg(comment_id)::uuid IS NULL AND sqlc.narg(task_id)::uuid IS NULL)
OR (sqlc.arg(state) = 'projected' AND sqlc.narg(task_id)::uuid IS NULL AND EXISTS (
    SELECT 1 FROM comment WHERE id=sqlc.narg(comment_id) AND workspace_id=sqlc.arg(workspace_id) AND issue_id=sqlc.arg(issue_id)
        AND author_type='system' AND github_notify_only
)) OR (sqlc.arg(state) = 'dispatched' AND EXISTS (
    SELECT 1 FROM agent_task_queue task JOIN comment ON comment.id = task.trigger_comment_id
    JOIN issue ON issue.id = task.issue_id
    WHERE task.id = sqlc.narg(task_id) AND issue.workspace_id = sqlc.arg(workspace_id) AND task.issue_id = sqlc.arg(issue_id)
      AND comment.id = sqlc.narg(comment_id) AND comment.workspace_id = sqlc.arg(workspace_id) AND comment.issue_id = sqlc.arg(issue_id)
)))
ON CONFLICT (workspace_id, revision_id, issue_id) DO NOTHING;

-- name: CreateGitHubNotifyComment :one
WITH touched_issue AS (
    UPDATE issue SET updated_at = now(), revision = revision + 1,
        last_activity_at = GREATEST(COALESCE(last_activity_at, updated_at), now())
    WHERE issue.id = sqlc.arg(issue_id) AND issue.workspace_id = sqlc.arg(workspace_id)
    RETURNING issue.id, issue.workspace_id, issue.revision
), inserted_comment AS (
    INSERT INTO comment (id,issue_id,workspace_id,author_type,author_id,content,type,github_notify_only)
    SELECT sqlc.arg(comment_id),touched_issue.id,touched_issue.workspace_id,'system','00000000-0000-0000-0000-000000000000',sqlc.arg(content),'comment',true
    FROM touched_issue RETURNING *
)
SELECT inserted_comment.*,touched_issue.revision AS issue_revision
FROM inserted_comment JOIN touched_issue ON touched_issue.id=inserted_comment.issue_id;

-- name: GetGitHubRouteIssue :one
SELECT issue.* FROM issue
JOIN issue_pull_request link ON link.issue_id=issue.id
JOIN github_pull_request pull ON pull.id=link.pull_request_id AND pull.workspace_id=issue.workspace_id
JOIN github_installation installation ON installation.workspace_id=issue.workspace_id AND installation.installation_id=pull.installation_id
WHERE issue.id=sqlc.arg(id) AND issue.workspace_id=sqlc.arg(workspace_id)
    AND pull.id=sqlc.arg(pull_request_id) AND pull.installation_id=sqlc.arg(installation_id)
FOR UPDATE OF issue;
-- name: DenyPendingGitHubWebhookRevisions :exec
UPDATE github_webhook_revision SET state = 'denied', reason = $6, updated_at = now()
WHERE workspace_id = $1 AND installation_id = $2 AND repository_id = $3
    AND object_kind = $4 AND object_id = $5
    AND state IN ('deferred', 'quarantined', 'unsigned', 'projected');

-- name: ListGitHubReconcilePullRequests :many
SELECT DISTINCT pull.id, pull.workspace_id, pull.installation_id, pull.repo_owner, pull.repo_name, pull.pr_number
FROM github_pull_request pull
JOIN issue_pull_request link ON link.pull_request_id = pull.id
JOIN github_installation installation ON installation.workspace_id = pull.workspace_id AND installation.installation_id = pull.installation_id
WHERE pull.state = 'open' AND pull.head_repo_same
ORDER BY pull.id;

-- name: GetLatestGitHubWebhookRevision :one
SELECT * FROM github_webhook_revision
WHERE workspace_id = $1 AND installation_id = $2 AND repository_id = $3
  AND object_kind = $4 AND object_id = $5
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: ListStaleGitHubWebhookItems :many
SELECT 'intake'::text AS item, delivery_guid AS ref, state, reason,
       EXTRACT(EPOCH FROM (now() - received_at))::bigint AS age_seconds
FROM github_webhook_intake
WHERE state = 'deferred' AND received_at < now() - sqlc.arg(min_age_seconds)::bigint * interval '1 second'
UNION ALL
SELECT 'revision'::text, id::text, state, reason,
       EXTRACT(EPOCH FROM (now() - created_at))::bigint
FROM github_webhook_revision
WHERE state = 'unsigned' AND created_at < now() - sqlc.arg(min_age_seconds)::bigint * interval '1 second'
ORDER BY 5 DESC;
