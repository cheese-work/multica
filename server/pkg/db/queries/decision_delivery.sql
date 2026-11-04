-- name: ListDecisionWakeCandidates :many
SELECT request.* FROM decision_request request
JOIN issue ON issue.id = request.issue_id AND issue.workspace_id = request.workspace_id
WHERE request.authority = 'human' AND request.status = 'answered' AND EXISTS (
    SELECT 1 FROM decision_answer answer
    WHERE answer.decision_request_id = request.id AND answer.disclaimed_at IS NULL
      AND answer.woken_at IS NULL AND answer.wake_at <= clock_timestamp()
)
AND (request.decision_intent_id IS NULL OR EXISTS (
    SELECT 1 FROM decision_intent intent
    WHERE intent.id = request.decision_intent_id AND intent.workspace_id = request.workspace_id
      AND (intent.group_woken_at IS NOT NULL OR (
          intent.sealed_at IS NOT NULL AND NOT EXISTS (
              SELECT 1 FROM decision_request member
              WHERE member.decision_intent_id = intent.id AND member.status <> 'cancelled'
                AND (member.status <> 'answered' OR NOT EXISTS (
                    SELECT 1 FROM decision_answer live
                    WHERE live.decision_request_id = member.id AND live.disclaimed_at IS NULL
                      AND live.woken_at IS NULL AND live.wake_at <= clock_timestamp()
                ))
          )
      ))
))
ORDER BY request.id
LIMIT LEAST(GREATEST(sqlc.arg('batch_size')::int, 0), 100);

-- name: DecisionDeliveryClock :one
SELECT clock_timestamp()::timestamptz;

-- name: ListDecisionDeliveryRequests :many
SELECT * FROM decision_request
WHERE authority = 'human' AND workspace_id = sqlc.arg('workspace_id')
  AND (decision_intent_id = sqlc.narg('intent_id')::uuid OR id = sqlc.narg('request_id')::uuid)
ORDER BY id;

-- name: ListDecisionDeliveryAnswers :many
SELECT answer.* FROM decision_answer answer
JOIN decision_request request ON request.authority = 'human' AND request.id = answer.decision_request_id
WHERE request.workspace_id = sqlc.arg('workspace_id') AND request.id = ANY(sqlc.arg('request_ids')::uuid[])
  AND request.status = 'answered' AND answer.disclaimed_at IS NULL
ORDER BY answer.id;

-- name: ListDecisionDeliveryTargets :many
SELECT target.* FROM decision_intent_target target
JOIN decision_intent intent ON intent.id = target.decision_intent_id
WHERE intent.workspace_id = sqlc.arg('workspace_id') AND intent.id = sqlc.arg('id')
ORDER BY target.human_user_id;

-- name: LockDecisionDeliveryTargets :many
SELECT target.* FROM decision_intent_target target
JOIN decision_intent intent ON intent.id = target.decision_intent_id
WHERE intent.workspace_id = sqlc.arg('workspace_id') AND intent.id = sqlc.arg('id')
ORDER BY target.human_user_id
FOR SHARE OF target;

-- name: LockDecisionRecipientRuntime :one
SELECT id FROM agent_runtime
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id')
FOR KEY SHARE;

-- name: LockDecisionRecipientAgent :one
SELECT id FROM agent
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id')
FOR KEY SHARE;

-- name: AcknowledgeDecisionWake :execrows
UPDATE decision_answer answer
SET woken_at = clock_timestamp(), wake_outcome = sqlc.arg('outcome')
FROM decision_request request
WHERE request.authority = 'human' AND request.id = answer.decision_request_id AND request.workspace_id = sqlc.arg('workspace_id')
  AND answer.id = sqlc.arg('id') AND answer.woken_at IS NULL AND answer.disclaimed_at IS NULL;

-- name: AcknowledgeDecisionGroupWake :execrows
UPDATE decision_intent SET group_woken_at = clock_timestamp()
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id')
  AND sealed_at IS NOT NULL AND group_woken_at IS NULL;

-- name: AppendDecisionDeliveryInputs :execrows
UPDATE agent_task_queue task
SET coalesced_comment_ids = (
        SELECT COALESCE(array_agg(DISTINCT comment_id ORDER BY comment_id), '{}')
        FROM unnest(task.coalesced_comment_ids || sqlc.arg('comment_ids')::uuid[]) AS inputs(comment_id)
        WHERE comment_id IS NOT NULL AND comment_id IS DISTINCT FROM task.trigger_comment_id
    ),
    handoff_note = concat_ws(E'\n', NULLIF(task.handoff_note, ''), sqlc.arg('summary')::text)
FROM agent owner
WHERE owner.id = task.agent_id AND owner.workspace_id = sqlc.arg('workspace_id')
  AND task.id = sqlc.arg('id') AND task.status IN ('queued', 'dispatched', 'running', 'waiting_local_directory');
