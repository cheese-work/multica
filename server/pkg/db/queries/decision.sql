-- name: CreateDecisionIntent :one
WITH stamp AS (SELECT clock_timestamp() AS opened_at)
INSERT INTO decision_intent (workspace_id, issue_id, task_id, agent_id, comment_id, opened_at, expires_at)
SELECT sqlc.arg('workspace_id'), sqlc.arg('issue_id'), sqlc.arg('task_id'), sqlc.arg('agent_id'), sqlc.arg('comment_id'), opened_at, opened_at + interval '3 minutes'
FROM stamp
RETURNING *;

-- name: CreateDecisionIntentTarget :one
INSERT INTO decision_intent_target (decision_intent_id, human_user_id)
VALUES (sqlc.arg('decision_intent_id'), sqlc.arg('human_user_id'))
RETURNING *;

-- name: StampCommentDecisionIntent :exec
UPDATE comment SET decision_intent_id = sqlc.arg('decision_intent_id')
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('comment_id');

-- name: AttachDecisionAskToComment :one
UPDATE comment SET decision_request_id = sqlc.arg('decision_request_id'), decision_comment_kind = 'ask', content = sqlc.arg('content')
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('comment_id')
RETURNING *;

-- name: CloseDecisionIntent :exec
UPDATE decision_intent
SET closed_at = COALESCE(closed_at, clock_timestamp()), close_reason = sqlc.arg('close_reason'),
    sealed_at = COALESCE(sealed_at, clock_timestamp())
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id');

-- name: SealDecisionIntent :exec
UPDATE decision_intent SET sealed_at = COALESCE(sealed_at, clock_timestamp())
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id');

-- name: DecisionIntentRefusal :one
SELECT CASE
    WHEN intent.sealed_at IS NOT NULL THEN 'sealed'
    WHEN intent.closed_at IS NOT NULL THEN 'closed'
    WHEN tag.id IS NULL OR tag.deleted_at IS NOT NULL THEN 'comment_deleted'
    WHEN task.id IS NULL OR task.status <> 'running' THEN 'task_stopped'
    WHEN clock_timestamp() >= intent.expires_at THEN 'expired'
    WHEN intent.consumed_at IS NOT NULL OR NOT EXISTS (
        SELECT 1 FROM decision_intent_target target
        WHERE target.decision_intent_id = intent.id AND target.consumed_at IS NULL
    ) THEN 'consumed'
    ELSE ''
END::text AS reason
FROM decision_intent intent
LEFT JOIN comment tag ON tag.id = intent.comment_id AND tag.issue_id = intent.issue_id AND tag.workspace_id = intent.workspace_id
LEFT JOIN agent_task_queue task ON task.id = intent.task_id
WHERE intent.workspace_id = sqlc.arg('workspace_id') AND intent.id = sqlc.arg('id');

-- name: CreateDecisionRequest :one
INSERT INTO decision_request (workspace_id, issue_id, asking_agent_id, human_user_id, chat_session_id, ping_message_id, issue_comment_id, ask, escalation_class, origin, decision_intent_id, source_comment_id, source_content_snapshot, request_fingerprint, options, ask_identity, ask_json, project_id)
VALUES (sqlc.arg('workspace_id'), sqlc.arg('issue_id'), sqlc.arg('asking_agent_id'), sqlc.arg('human_user_id'), sqlc.arg('chat_session_id'), sqlc.narg('ping_message_id'), sqlc.narg('issue_comment_id'), sqlc.arg('ask'), sqlc.arg('escalation_class'), sqlc.arg('origin'), sqlc.narg('decision_intent_id'), sqlc.narg('source_comment_id'), sqlc.arg('source_content_snapshot'), sqlc.arg('request_fingerprint'), COALESCE(sqlc.narg('options')::jsonb, '[]'::jsonb), sqlc.arg('ask_identity'), sqlc.narg('ask_json')::jsonb, sqlc.narg('project_id'))
RETURNING *;

-- name: CreateDecisionAnswer :one
INSERT INTO decision_answer (decision_request_id, answered_by_task_id, source_chat_session_id, chat_message_ids, source_comment_id, content_snapshot, effective_at, issue_comment_id, wake_at, chosen_option_index, reply_text, context_message_id, excerpted, answered_label)
VALUES (sqlc.arg('decision_request_id'), sqlc.narg('answered_by_task_id'), sqlc.narg('source_chat_session_id'), sqlc.arg('chat_message_ids'), sqlc.narg('source_comment_id'), sqlc.arg('content_snapshot'), sqlc.arg('effective_at'), sqlc.narg('issue_comment_id'), sqlc.narg('wake_at'), sqlc.narg('chosen_option_index'), sqlc.narg('reply_text'), sqlc.narg('context_message_id'), sqlc.arg('excerpted')::boolean, sqlc.narg('answered_label'))
RETURNING *;

-- name: GetDecisionRequest :one
SELECT * FROM decision_request
WHERE authority = 'human' AND workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id');

-- name: GetDecisionAnswer :one
SELECT answer.* FROM decision_answer answer
JOIN decision_request request ON request.authority = 'human' AND request.id = answer.decision_request_id
WHERE request.workspace_id = sqlc.arg('workspace_id') AND answer.id = sqlc.arg('id');

-- name: MarkDecisionRequestAnswered :one
UPDATE decision_request SET status = 'answered'
WHERE authority = 'human' AND workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id') AND status = 'open'
RETURNING *;

-- name: DecisionRecordEffectiveAt :one
SELECT (clock_timestamp() + CASE WHEN escalation_class = 'reversible' THEN interval '0 seconds' ELSE interval '10 minutes' END)::timestamptz AS effective_at
FROM decision_request WHERE authority = 'human' AND workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id');

-- name: WithdrawDecisionAnswer :one
UPDATE decision_answer answer
SET disclaimed_at = clock_timestamp(), disclaim_reason = sqlc.arg('disclaim_reason'),
    disclaimed_by_user_id = sqlc.narg('disclaimed_by_user_id'),
    disclaim_comment_posted_at = clock_timestamp(), withdrawal_comment_id = sqlc.arg('withdrawal_comment_id'),
    wake_at = CASE WHEN woken_at IS NULL THEN NULL ELSE wake_at END,
    disclaimed_by_agent_id = sqlc.narg('disclaimed_by_agent_id'),
    disclaimed_by_task_id = sqlc.narg('disclaimed_by_task_id'),
    veto_reason = sqlc.narg('veto_reason'),
    retraction_delivered_at = CASE WHEN sqlc.arg('suppress_retraction')::boolean THEN clock_timestamp() ELSE retraction_delivered_at END,
    retraction_outcome = CASE WHEN sqlc.arg('suppress_retraction')::boolean THEN 'suppressed_agent_veto' ELSE retraction_outcome END
FROM decision_request request
WHERE request.authority = 'human' AND request.id = answer.decision_request_id AND request.workspace_id = sqlc.arg('workspace_id')
  AND answer.id = sqlc.arg('id') AND answer.disclaimed_at IS NULL
RETURNING answer.*;

-- name: ReopenDecisionRequest :one
UPDATE decision_request SET status = 'open'
WHERE authority = 'human' AND workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id') AND status = 'answered'
RETURNING *;

-- name: RaiseDecisionClass :one
UPDATE decision_request SET escalation_class = sqlc.arg('escalation_class')
WHERE authority = 'human' AND workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id')
RETURNING *;

-- name: CreateDecisionClassChange :exec
INSERT INTO decision_class_change (decision_request_id, from_class, to_class, changed_by_user_id)
SELECT id, sqlc.arg('from_class'), sqlc.arg('to_class'), sqlc.arg('changed_by_user_id')
FROM decision_request
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id');

-- name: LockDecisionLiveAnswer :one
SELECT answer.* FROM decision_answer answer
JOIN decision_request request ON request.authority = 'human' AND request.id = answer.decision_request_id
WHERE request.workspace_id = sqlc.arg('workspace_id') AND request.id = sqlc.arg('id')
  AND answer.disclaimed_at IS NULL
FOR UPDATE OF answer;

-- name: HoldDecisionAnswerForClass :one
WITH hold AS (SELECT clock_timestamp() + interval '10 minutes' AS held_until)
UPDATE decision_answer answer SET
    effective_at = hold.held_until,
    wake_at = CASE WHEN wake_at IS NOT NULL THEN hold.held_until ELSE NULL END
FROM decision_request request, hold
WHERE request.authority = 'human' AND request.id = answer.decision_request_id AND request.workspace_id = sqlc.arg('workspace_id')
  AND answer.id = sqlc.arg('id') AND answer.disclaimed_at IS NULL AND answer.woken_at IS NULL
RETURNING answer.*;

-- name: CancelWithdrawnDecisionRequest :one
UPDATE decision_request SET status = 'cancelled', cancel_reason = sqlc.arg('cancel_reason'),
    cancel_notice_posted_at = clock_timestamp()
WHERE authority = 'human' AND workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id') AND status = 'answered'
RETURNING *;

-- name: DecisionOpenLaneExists :one
SELECT EXISTS (
    SELECT 1 FROM decision_request
    WHERE authority = 'human' AND workspace_id = sqlc.arg('workspace_id') AND chat_session_id = sqlc.arg('session_id') AND status = 'open'
)::boolean AS present;

-- name: ListDecisionSessionReferences :many
SELECT DISTINCT COALESCE(workspace.issue_prefix || '-' || issue.number::text, request.issue_id::text)::text AS issue_key
FROM decision_request request
LEFT JOIN issue ON issue.id = request.issue_id AND issue.workspace_id = request.workspace_id
LEFT JOIN workspace ON workspace.id = request.workspace_id
WHERE request.authority = 'human' AND request.workspace_id = sqlc.arg('workspace_id') AND (
    (request.status = 'open' AND request.chat_session_id = sqlc.arg('session_id'))
    OR EXISTS (
        SELECT 1 FROM decision_answer answer
        WHERE answer.decision_request_id = request.id AND answer.source_chat_session_id = sqlc.arg('session_id')
    )
)
ORDER BY issue_key;

-- name: ListDecisionRequestsByIssue :many
SELECT * FROM decision_request
WHERE authority = 'human' AND workspace_id = sqlc.arg('workspace_id') AND issue_id = sqlc.arg('issue_id')
ORDER BY created_at, id;

-- name: ListDecisionPingRetries :many
SELECT * FROM decision_request
WHERE authority = 'human' AND origin = 'asked' AND ping_notified_at IS NULL
ORDER BY created_at, id
LIMIT $1;

-- name: MarkDecisionPingNotified :one
UPDATE decision_request
SET ping_notified_at = clock_timestamp()
WHERE authority = 'human' AND workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id')
  AND ping_notified_at IS NULL
RETURNING *;

-- name: ConsumeDecisionIntentTarget :execrows
UPDATE decision_intent_target target
SET consumed_at = clock_timestamp(), consumed_by_request_id = sqlc.arg('request_id')
FROM decision_intent intent
WHERE intent.id = target.decision_intent_id AND intent.workspace_id = sqlc.arg('workspace_id')
  AND intent.id = sqlc.arg('decision_intent_id') AND target.human_user_id = sqlc.arg('human_user_id')
  AND target.consumed_at IS NULL;

-- name: CompleteDecisionIntent :exec
UPDATE decision_intent intent
SET consumed_at = clock_timestamp(), consumed_by_request_id = sqlc.arg('request_id'),
    sealed_at = clock_timestamp()
WHERE intent.workspace_id = sqlc.arg('workspace_id') AND intent.id = sqlc.arg('id')
  AND intent.consumed_at IS NULL AND intent.sealed_at IS NULL AND intent.closed_at IS NULL
  AND NOT EXISTS (
      SELECT 1 FROM decision_intent_target target
      WHERE target.decision_intent_id = intent.id AND target.consumed_at IS NULL
  );

-- name: GetDecisionIntentForTask :one
SELECT * FROM decision_intent
WHERE workspace_id = sqlc.arg('workspace_id') AND issue_id = sqlc.arg('issue_id')
  AND task_id = sqlc.arg('task_id') AND agent_id = sqlc.arg('agent_id')
ORDER BY opened_at DESC, id DESC
LIMIT 1;

-- name: ListDecisionIntentTargets :many
SELECT target.* FROM decision_intent_target target
JOIN decision_intent intent ON intent.id = target.decision_intent_id
WHERE intent.workspace_id = sqlc.arg('workspace_id') AND intent.id = sqlc.arg('decision_intent_id')
ORDER BY target.human_user_id;

-- name: ListDecisionLanes :many
-- With issue_id, lanes that name the issue (a message mention or a decision
-- request on it) rank first, newest reference first, before the limit applies.
-- session_id narrows the result to one lane for an eligibility check. A lane
-- of topic_id is listed whichever agent stewards it.
SELECT session.id, session.title, session.updated_at, session.decision_topic_id, ref.referenced_at::timestamptz AS referenced_at
FROM chat_session session
LEFT JOIN LATERAL (
    SELECT max(refs.referenced_at) AS referenced_at FROM (
        SELECT message.created_at AS referenced_at
        FROM chat_message message
        WHERE message.chat_session_id = session.id
          AND message.role IN ('user', 'assistant') AND message.message_kind = 'message'
          AND message.failure_reason IS NULL
          AND lower(message.content) LIKE '%](mention://issue/' || lower(sqlc.narg('issue_id')::uuid::text) || ')%'
        UNION ALL
        SELECT request.created_at
        FROM decision_request request
        WHERE request.authority = 'human' AND request.workspace_id = session.workspace_id AND request.issue_id = sqlc.narg('issue_id')::uuid
          AND request.chat_session_id = session.id
    ) refs
) ref ON sqlc.narg('issue_id')::uuid IS NOT NULL
WHERE session.workspace_id = sqlc.arg('workspace_id')
  AND (session.agent_id = sqlc.arg('agent_id') OR session.decision_topic_id = sqlc.narg('topic_id')::uuid)
  AND session.creator_id = sqlc.arg('creator_id') AND session.status = 'active'
  AND session.channel_origin_at IS NULL AND NOT session.is_agent_intro
  AND NOT EXISTS (SELECT 1 FROM channel_chat_session_binding binding WHERE binding.chat_session_id = session.id)
  AND (sqlc.narg('session_id')::uuid IS NULL OR session.id = sqlc.narg('session_id')::uuid)
ORDER BY ref.referenced_at DESC NULLS LAST, session.updated_at DESC, session.id DESC
LIMIT 10;

-- name: GetDecisionTopicLane :one
-- The one DM a person has for a decision topic.
SELECT session.id FROM chat_session session
WHERE session.workspace_id = sqlc.arg('workspace_id') AND session.creator_id = sqlc.arg('creator_id')
  AND session.decision_topic_id = sqlc.arg('topic_id') AND session.status = 'active'
  AND session.channel_origin_at IS NULL AND NOT session.is_agent_intro
  AND NOT EXISTS (SELECT 1 FROM channel_chat_session_binding binding WHERE binding.chat_session_id = session.id)
ORDER BY session.created_at, session.id
LIMIT 1;

-- name: LockDecisionTopicLane :exec
-- Serializes the find-or-create of a person's DM for one topic.
SELECT pg_advisory_xact_lock(hashtextextended('decision-topic:' || sqlc.arg('workspace_id')::uuid::text || ':' || sqlc.arg('creator_id')::uuid::text || ':' || sqlc.arg('topic_id')::uuid::text, 0));

-- name: ListDecisionTopicSessions :many
SELECT chat_session.id, chat_session.creator_id FROM chat_session
WHERE chat_session.workspace_id = sqlc.arg('workspace_id') AND chat_session.decision_topic_id = sqlc.arg('topic_id') AND chat_session.status = 'active' AND chat_session.agent_id <> sqlc.arg('agent_id');

-- name: MoveDecisionSessionSteward :execrows
UPDATE chat_session SET agent_id = sqlc.arg('agent_id'), runtime_id = (SELECT runtime_id FROM agent WHERE id = sqlc.arg('agent_id'))
WHERE chat_session.workspace_id = sqlc.arg('workspace_id') AND chat_session.id = sqlc.arg('id') AND chat_session.status = 'active';

-- name: OpenDecisionAsksSince :many
SELECT ask, ask_identity, created_at FROM decision_request
WHERE authority = 'human' AND workspace_id = sqlc.arg('workspace_id') AND issue_id = sqlc.arg('issue_id')
  AND asking_agent_id = sqlc.arg('asking_agent_id') AND human_user_id = sqlc.arg('human_user_id')
  AND status = 'open' AND created_at > now() - interval '24 hours'
ORDER BY created_at DESC;

-- name: ListDecisionLaneMentions :many
WITH scoped_mentions AS MATERIALIZED (
    SELECT message.chat_session_id, message.id AS message_id, message.created_at AS mentioned_at,
        session.workspace_id, message.content, mention.parts
    FROM chat_message message
    JOIN chat_session session ON session.id = message.chat_session_id
    CROSS JOIN LATERAL regexp_matches(message.content, '(\[((?:(?!\]\()[^\n])+?)\]\(mention://issue/([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\))', 'g') AS mention(parts)
    WHERE session.workspace_id = sqlc.arg('workspace_id')
      AND session.agent_id = sqlc.arg('agent_id') AND session.creator_id = sqlc.arg('creator_id')
      AND session.id = ANY(sqlc.arg('session_ids')::uuid[])
      AND session.status = 'active' AND session.channel_origin_at IS NULL AND NOT session.is_agent_intro
      AND NOT EXISTS (SELECT 1 FROM channel_chat_session_binding binding WHERE binding.chat_session_id = session.id)
      AND message.role IN ('user', 'assistant') AND message.message_kind = 'message'
      AND message.content LIKE '%](mention://issue/%'
      AND message.failure_reason IS NULL
      AND NOT EXISTS (SELECT 1 FROM decision_request WHERE authority = 'human' AND ping_message_id = message.id)
), newest AS (
    SELECT DISTINCT ON (mention.chat_session_id, issue.id)
        mention.chat_session_id, issue.id AS issue_id,
        (workspace.issue_prefix || '-' || issue.number::text)::text AS issue_key,
        issue.title AS issue_title, mention.mentioned_at, mention.message_id,
        mention.content, mention.parts[1] AS mention_link
    FROM scoped_mentions mention
    JOIN workspace ON workspace.id = mention.workspace_id
    JOIN issue ON issue.id = mention.parts[3]::uuid AND issue.workspace_id = mention.workspace_id
    ORDER BY mention.chat_session_id, issue.id, mention.mentioned_at DESC, mention.message_id DESC
), ranked AS (
    SELECT *, row_number() OVER (PARTITION BY chat_session_id ORDER BY mentioned_at DESC, message_id DESC, issue_id DESC) AS mention_rank
    FROM newest
)
SELECT chat_session_id, issue_id, issue_key, issue_title, mentioned_at,
    (right(regexp_replace(split_part(content, mention_link, 1), '\[((?:(?!\]\()[^\n])+?)\]\([^)]*\)', '\1', 'g'), 40)
     || left(regexp_replace(substring(content FROM strpos(content, mention_link) + char_length(mention_link)), '\[((?:(?!\]\()[^\n])+?)\]\([^)]*\)', '\1', 'g'), 60))::text AS snippet
FROM ranked WHERE mention_rank <= 20
ORDER BY chat_session_id, mention_rank;

-- name: DecisionLaneWindowOpen :one
SELECT EXISTS (
    SELECT 1 FROM decision_intent intent
    JOIN decision_intent_target target ON target.decision_intent_id = intent.id
    JOIN comment tag ON tag.id = intent.comment_id AND tag.issue_id = intent.issue_id AND tag.workspace_id = intent.workspace_id
    JOIN agent_task_queue task ON task.id = intent.task_id AND task.agent_id = intent.agent_id AND task.issue_id = intent.issue_id
    JOIN agent asking ON asking.id = intent.agent_id AND asking.workspace_id = intent.workspace_id
    JOIN member human ON human.user_id = target.human_user_id AND human.workspace_id = intent.workspace_id
    WHERE intent.workspace_id = sqlc.arg('workspace_id') AND intent.id = sqlc.arg('decision_intent_id')
      AND target.human_user_id = sqlc.arg('human_user_id') AND target.consumed_at IS NULL
      AND intent.sealed_at IS NULL AND intent.closed_at IS NULL AND intent.consumed_at IS NULL
      AND tag.deleted_at IS NULL AND task.status = 'running' AND task.chat_session_id IS NULL
      AND asking.archived_at IS NULL AND clock_timestamp() < intent.expires_at
)::boolean AS open;

-- name: LockDecisionSession :one
SELECT id FROM chat_session
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id')
FOR UPDATE;

-- name: LockDecisionIssue :one
SELECT id FROM issue
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id')
FOR NO KEY UPDATE;

-- name: LockDecisionCallingTask :one
SELECT task.id FROM agent_task_queue task
JOIN agent owner ON owner.id = task.agent_id
WHERE owner.workspace_id = sqlc.arg('workspace_id') AND task.id = sqlc.arg('id')
FOR SHARE OF task NOWAIT;

-- name: LockDecisionDeliveryTask :one
SELECT task.id FROM agent_task_queue task
JOIN agent owner ON owner.id = task.agent_id
WHERE owner.workspace_id = sqlc.arg('workspace_id') AND task.id = sqlc.arg('id')
FOR UPDATE OF task NOWAIT;

-- name: LockDecisionAskingAgent :one
SELECT id FROM agent
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id')
FOR SHARE NOWAIT;

-- name: LockDecisionMember :one
SELECT id FROM member
WHERE workspace_id = sqlc.arg('workspace_id') AND user_id = sqlc.arg('user_id')
FOR KEY SHARE NOWAIT;

-- name: LockDecisionSourceMessages :many
SELECT message.id FROM chat_message message
JOIN chat_session session ON session.id = message.chat_session_id
WHERE session.workspace_id = sqlc.arg('workspace_id') AND message.id = ANY(sqlc.arg('message_ids')::uuid[])
ORDER BY message.id
FOR KEY SHARE OF message NOWAIT;

-- name: LockDecisionIntent :one
SELECT * FROM decision_intent
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id')
FOR UPDATE;

-- name: LockDecisionPendingIntent :one
SELECT * FROM decision_intent
WHERE workspace_id = sqlc.arg('workspace_id') AND task_id = sqlc.arg('task_id')
  AND consumed_at IS NULL AND closed_at IS NULL
FOR UPDATE;

-- name: LockDecisionIntentTarget :one
SELECT target.* FROM decision_intent_target target
JOIN decision_intent intent ON intent.id = target.decision_intent_id
WHERE intent.workspace_id = sqlc.arg('workspace_id') AND target.decision_intent_id = sqlc.arg('decision_intent_id') AND target.human_user_id = sqlc.arg('human_user_id')
FOR UPDATE OF target;

-- name: LockDecisionRequest :one
SELECT * FROM decision_request
WHERE authority = 'human' AND workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id')
FOR UPDATE;

-- name: LockDecisionAnswer :one
SELECT answer.* FROM decision_answer answer
JOIN decision_request request ON request.authority = 'human' AND request.id = answer.decision_request_id
WHERE request.workspace_id = sqlc.arg('workspace_id') AND answer.id = sqlc.arg('id')
FOR UPDATE OF answer;

-- name: GetDecisionIntent :one
SELECT * FROM decision_intent
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id');

-- name: DecisionAnswerSourceRemoved :one
SELECT (EXISTS (
    SELECT 1 FROM unnest(answer.chat_message_ids) AS source(id)
    WHERE NOT EXISTS (SELECT 1 FROM chat_message message WHERE message.id = source.id)
) OR (answer.source_comment_id IS NOT NULL AND NOT EXISTS (
    SELECT 1 FROM comment source WHERE source.id = answer.source_comment_id
      AND source.workspace_id = request.workspace_id AND source.deleted_at IS NULL
)))::boolean AS removed
FROM decision_answer answer
JOIN decision_request request ON request.authority = 'human' AND request.id = answer.decision_request_id
WHERE request.workspace_id = sqlc.arg('workspace_id') AND answer.id = sqlc.arg('id');

-- name: ListDecisionSourceChecks :many
SELECT request.* FROM decision_request request
JOIN issue ON issue.id = request.issue_id AND issue.workspace_id = request.workspace_id
JOIN chat_session session ON session.id = request.chat_session_id AND session.workspace_id = request.workspace_id
WHERE request.authority = 'human' AND EXISTS (
    SELECT 1 FROM decision_answer answer
    WHERE answer.decision_request_id = request.id AND answer.disclaimed_at IS NULL
      AND (EXISTS (
          SELECT 1 FROM unnest(answer.chat_message_ids) AS source(id)
          WHERE NOT EXISTS (SELECT 1 FROM chat_message message WHERE message.id = source.id)
      ) OR (answer.source_comment_id IS NOT NULL AND NOT EXISTS (
          SELECT 1 FROM comment source WHERE source.id = answer.source_comment_id
            AND source.workspace_id = request.workspace_id AND source.deleted_at IS NULL
      )))
)
ORDER BY request.id
LIMIT LEAST(GREATEST(sqlc.arg('batch_size')::int, 0), 100);

-- name: ListDecisionSealIntents :many
SELECT intent.* FROM decision_intent intent
JOIN issue ON issue.id = intent.issue_id AND issue.workspace_id = intent.workspace_id
LEFT JOIN comment tag ON tag.id = intent.comment_id AND tag.issue_id = intent.issue_id AND tag.workspace_id = intent.workspace_id
LEFT JOIN agent_task_queue task ON task.id = intent.task_id
WHERE intent.sealed_at IS NULL AND (
    clock_timestamp() >= intent.expires_at OR task.id IS NULL OR task.status <> 'running'
    OR tag.id IS NULL OR tag.deleted_at IS NOT NULL
)
ORDER BY intent.id
LIMIT LEAST(GREATEST(sqlc.arg('batch_size')::int, 0), 100);

-- name: ListDecisionCards :many
-- One row per answer that a DM bubble was quoted into, or that a comment/
-- reference message names. Matches on the source messages (GIN index), on
-- explicit answer ids and on the source comment a member answered in, so one
-- call serves the issue timeline and the DM list.
SELECT answer.id AS answer_id, answer.chat_message_ids, answer.source_chat_session_id, answer.source_comment_id,
       answer.issue_comment_id, answer.effective_at, answer.woken_at, answer.wake_outcome, answer.disclaimed_at,
       answer.chosen_option_index, answer.excerpted, answer.answered_label, answer.content_snapshot,
       answer.created_at AS answered_at, request.options AS request_options,
       request.issue_id, request.escalation_class, request.human_user_id, request.asking_agent_id,
       request.chat_session_id AS request_chat_session_id, request.ping_message_id AS request_ping_message_id,
       issue.number AS issue_number, workspace.issue_prefix
FROM decision_answer answer
JOIN decision_request request ON request.authority = 'human' AND request.id = answer.decision_request_id
JOIN issue ON issue.id = request.issue_id
JOIN workspace ON workspace.id = request.workspace_id
WHERE request.workspace_id = sqlc.arg('workspace_id')
  AND (answer.id = ANY(sqlc.arg('answer_ids')::uuid[]) OR answer.chat_message_ids && sqlc.arg('message_ids')::uuid[]
       OR answer.source_comment_id = ANY(sqlc.arg('source_comment_ids')::uuid[]))
ORDER BY answer.created_at, answer.id;

-- name: ListChatDecisionContext :many
-- The asks a bare reply in a DM may be answering: open, asked, in this
-- workspace and DM, with the ping before the first reply being claimed. The ping
-- is an assistant row no task produced, so it is in neither the resumed provider
-- session nor the user-only prompt input. Every eligible ask is returned, so the
-- run can explain any open card. Answered or cancelled asks, newer pings and
-- decision notes are not eligible.
SELECT request.id, request.issue_id, request.issue_comment_id, request.ping_message_id, request.ask, request.options,
       COALESCE(workspace.issue_prefix || '-' || issue.number::text, request.issue_id::text)::text AS issue_key,
       COALESCE(issue.title, '')::text AS issue_title
FROM decision_request request
JOIN chat_message ping ON ping.id = request.ping_message_id AND ping.chat_session_id = request.chat_session_id
LEFT JOIN issue ON issue.id = request.issue_id AND issue.workspace_id = request.workspace_id
LEFT JOIN workspace ON workspace.id = request.workspace_id
WHERE request.authority = 'human' AND request.workspace_id = sqlc.arg('workspace_id') AND request.chat_session_id = sqlc.arg('chat_session_id')
  AND request.origin = 'asked' AND request.status = 'open'
  AND ping.created_at < sqlc.arg('before')
ORDER BY ping.created_at DESC, request.id DESC;

-- name: ListChatDecisionReferences :many
-- Server-authored decision notes (cancellations, recorded answers) posted after
-- the session's previous task claimed its input; see ListChatDecisionContext
-- for why the boundary is the claim. They reference a request by text, not by
-- id, so they are read by message rather than through decision_request.
SELECT message.content FROM chat_message message
WHERE message.chat_session_id = sqlc.arg('chat_session_id') AND message.message_kind IN ('decision_reference', 'decision_cancel_notice')
  AND message.created_at > COALESCE((SELECT max(prior.dispatched_at) FROM agent_task_queue prior WHERE prior.chat_session_id = message.chat_session_id AND prior.id <> sqlc.arg('task_id') AND NOT (prior.task_kind = 'compact' AND prior.compact_reason IS NOT NULL)), '-infinity')
ORDER BY message.created_at DESC, message.id DESC
LIMIT sqlc.arg('row_limit');

-- name: GetLastAssistantMessageBefore :one
-- The agent's own last reply before a message: the options a bare reply chose
-- between, attestable with --context-message.
SELECT id FROM chat_message
WHERE chat_session_id = sqlc.arg('session_id') AND role = 'assistant' AND message_kind = 'message'
  AND created_at < sqlc.arg('before')
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: CreateDecisionCardAnswer :one
-- A human's click on a decision card (approve or reject of a decision_action).
-- No task, chat or comment source: the answering turn does not exist, so no
-- finalizer will ever schedule it. The click schedules the wake itself.
INSERT INTO decision_answer (decision_request_id, answered_by_user_id, answer_source, chat_message_ids, content_snapshot, effective_at, issue_comment_id, wake_at)
VALUES (sqlc.arg('decision_request_id'), sqlc.arg('answered_by_user_id'), 'card', '{}', sqlc.arg('content_snapshot'), sqlc.arg('effective_at'), sqlc.narg('issue_comment_id'), GREATEST(sqlc.arg('effective_at')::timestamptz, clock_timestamp()))
RETURNING *;

-- name: DecisionHumanAuthority :one
-- is_owner: the person holds the workspace owner role. is_delegate: the person is
-- listed in decision_delegates of the project the issue belongs to.
SELECT (member.role = 'owner')::boolean AS is_owner,
    COALESCE((
        SELECT project.decision_delegates @> ARRAY[member.user_id]
        FROM project
        WHERE project.id = sqlc.narg('project_id') AND project.workspace_id = member.workspace_id
    ), false)::boolean AS is_delegate
FROM member
WHERE member.workspace_id = sqlc.arg('workspace_id') AND member.user_id = sqlc.arg('user_id');
