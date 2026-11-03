-- name: ListAskRepairEvents :many
-- Open block events of an ended, non-chat run that the recovery notice sent to
-- repair and no repair pass has handled yet.
SELECT event.* FROM ask_guard_event event
JOIN agent_task_queue task ON task.id = event.task_id AND task.status IN ('completed', 'failed', 'cancelled')
WHERE event.mode = 'block' AND event.cleared_at IS NULL AND event.repair_reason IS NOT NULL AND event.repair_handled_at IS NULL
  AND task.chat_session_id IS NULL
ORDER BY event.id
LIMIT LEAST(GREATEST(sqlc.arg('batch_size')::int, 0), 100);

-- name: SetAskGuardEventRepairHandled :exec
UPDATE ask_guard_event SET repair_handled_at = clock_timestamp()
WHERE id = $1 AND workspace_id = $2 AND repair_handled_at IS NULL;

-- name: InsertAskRepairRoot :one
-- The unique id is the admission: a second event of the same root conflicts
-- and returns no row.
INSERT INTO ask_repair_root (id, workspace_id, issue_id, agent_id)
VALUES ($1, $2, $3, $4)
ON CONFLICT (id) DO NOTHING
RETURNING *;

-- name: LockAskRepairRoot :one
SELECT * FROM ask_repair_root WHERE id = $1 AND workspace_id = $2 FOR UPDATE;

-- name: SetAskRepairRootTask :exec
UPDATE ask_repair_root SET repair_task_id = $3 WHERE id = $1 AND workspace_id = $2;

-- name: FailAskRepairRoot :one
-- Only an open root fails, once; the caller posts the one comment on a row.
UPDATE ask_repair_root SET state = 'failed', failure_reason = $3, failure_comment_id = $4, failed_at = clock_timestamp()
WHERE id = $1 AND workspace_id = $2 AND state = 'admitted'
RETURNING *;

-- name: SettleAskRepairRoot :exec
UPDATE ask_repair_root SET state = 'settled', settled_at = clock_timestamp()
WHERE id = $1 AND workspace_id = $2 AND state = 'admitted';

-- name: ListEndedAskRepairRoots :many
-- Admitted roots whose repair task has ended: the work for the settle pass.
SELECT root.* FROM ask_repair_root root
JOIN agent_task_queue task ON task.id = root.repair_task_id AND task.status IN ('completed', 'failed', 'cancelled')
WHERE root.state = 'admitted'
ORDER BY root.id
LIMIT LEAST(GREATEST(sqlc.arg('batch_size')::int, 0), 100);

-- name: GetAskGuardEventInWorkspace :one
SELECT * FROM ask_guard_event WHERE id = $1 AND workspace_id = $2;

-- name: CountOpenAskRepairRootEvents :one
-- Open block events a repair run left on a root, besides the root's own event.
SELECT count(*) FROM ask_guard_event
WHERE root_id = $1 AND workspace_id = $2 AND id <> $1 AND mode = 'block' AND cleared_at IS NULL;

-- name: ListFailedAskRepairRoots :many
-- The owned failures for the decision brief, with the root event's stored
-- source. Source is empty when it was deleted.
SELECT root.id, root.issue_id, root.agent_id, root.failure_reason, root.failed_at,
       (workspace.issue_prefix || '-' || issue.number)::text AS identifier,
       COALESCE(event.source_text, comment.content, '')::text AS source,
       COALESCE(agent.name, root.agent_id::text)::text AS agent_name
FROM ask_repair_root root
LEFT JOIN agent ON agent.id = root.agent_id
JOIN issue ON issue.id = root.issue_id AND issue.workspace_id = root.workspace_id
JOIN workspace ON workspace.id = root.workspace_id
LEFT JOIN ask_guard_event event ON event.id = root.id AND event.workspace_id = root.workspace_id
LEFT JOIN decision_intent intent ON intent.id = event.intent_id AND intent.workspace_id = event.workspace_id
LEFT JOIN comment ON comment.id = intent.comment_id AND comment.issue_id = event.issue_id AND comment.workspace_id = event.workspace_id
WHERE root.workspace_id = $1 AND root.state = 'failed'
ORDER BY root.failed_at DESC
LIMIT 100;
