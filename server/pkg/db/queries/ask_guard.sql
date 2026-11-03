-- name: InsertAskGuardEvent :one
-- A conflict on the operation key returns no row; the caller reads the
-- existing event with GetAskGuardEventByOperation.
INSERT INTO ask_guard_event (id, workspace_id, issue_id, task_id, operation_kind, payload_sha256, mode, member_id, source_text, root_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, COALESCE((SELECT root.id FROM ask_repair_root root WHERE root.repair_task_id = $4), $1))
ON CONFLICT (task_id, operation_kind, payload_sha256, mode) DO NOTHING
RETURNING *;

-- name: GetAskGuardEventByOperation :one
SELECT * FROM ask_guard_event
WHERE task_id = $1 AND operation_kind = $2 AND payload_sha256 = $3 AND mode = $4;

-- name: ListAskGuardIntentTargets :many
-- S3: every unconsumed target of a sealed intent whose run has ended and that
-- has no event yet, whether or not other targets of the intent were consumed.
-- No age cutoff: a run can end long after its intent was sealed, and a worker
-- can be down for days. The NOT EXISTS makes a handled intent drop out; the
-- batch limit bounds one pass.
SELECT intent.id AS intent_id, intent.workspace_id, intent.issue_id, intent.task_id, target.human_user_id,
       (workspace.settings->>'ask_guard_mode')::text AS mode
FROM decision_intent intent
JOIN workspace ON workspace.id = intent.workspace_id AND workspace.settings->>'ask_guard_mode' IN ('warn', 'block')
JOIN decision_intent_target target ON target.decision_intent_id = intent.id AND target.consumed_at IS NULL
JOIN agent_task_queue task ON task.id = intent.task_id AND task.status IN ('completed', 'failed', 'cancelled')
JOIN issue ON issue.id = intent.issue_id AND issue.workspace_id = intent.workspace_id
WHERE intent.sealed_at IS NOT NULL AND intent.closed_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM ask_guard_event event WHERE event.intent_id = intent.id AND event.member_id = target.human_user_id)
ORDER BY intent.id, target.human_user_id
LIMIT LEAST(GREATEST(sqlc.arg('batch_size')::int, 0), 100);

-- name: InsertAskGuardIntentTargetEvent :one
-- One event per intent target. A conflict (another pass won) returns no row.
INSERT INTO ask_guard_event (id, workspace_id, issue_id, task_id, operation_kind, payload_sha256, mode, intent_id, member_id, root_id)
VALUES ($1, $2, $3, $4, 'intent_target', $5, $6, $7, $8, COALESCE((SELECT root.id FROM ask_repair_root root WHERE root.repair_task_id = $4), $1))
ON CONFLICT DO NOTHING
RETURNING *;

-- name: ListAskGuardRecoverableEvents :many
-- Open block events whose source task has ended: the work for the recovery
-- notice. The task and the event can commit in either order; a task still
-- running is picked up by a later pass.
SELECT event.* FROM ask_guard_event event
JOIN agent_task_queue task ON task.id = event.task_id AND task.status IN ('completed', 'failed', 'cancelled')
WHERE event.mode = 'block' AND event.cleared_at IS NULL
ORDER BY event.id
LIMIT LEAST(GREATEST(sqlc.arg('batch_size')::int, 0), 100);

-- name: GetAskGuardEventForMember :one
-- The event a request recovers: same issue, same member. A request for A
-- never matches an event for B.
SELECT * FROM ask_guard_event
WHERE id = $1 AND workspace_id = $2 AND issue_id = $3 AND member_id = $4;

-- name: ClearAskGuardEvent :exec
UPDATE ask_guard_event SET cleared_at = clock_timestamp(), cleared_by_request_id = $2
WHERE id = $1 AND cleared_at IS NULL;
