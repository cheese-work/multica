-- Read side of DM attention (CHE-4145, plan r4). The projection itself is pure Go
-- (service/chat_attention.go); these queries only load its inputs for one human.

-- name: ListChatAttentionChains :many
-- One row per chain: its canonical DM (the session nothing continued into).
SELECT id AS canonical_id, chain_root_session_id AS root_id, (status = 'archived')::boolean AS archived
FROM chat_session
WHERE workspace_id = $1 AND creator_id = $2 AND continued_into_session_id IS NULL
  AND chain_root_session_id IS NOT NULL;

-- name: ListChatAttentionItems :many
-- Open ask / later / unseen rows. waiting is true while the task consuming the targeted send is live (rule 3).
SELECT i.id, i.chain_root_session_id, i.kind, i.opened_at, i.due_at,
       (i.waiting_message_id IS NOT NULL AND COALESCE(t.status NOT IN ('completed', 'failed', 'cancelled'), false))::boolean AS waiting
FROM chat_attention_item i
LEFT JOIN agent_task_queue t ON t.id = i.waiting_task_id
WHERE i.user_id = $1 AND i.workspace_id = $2 AND i.resolved_at IS NULL;

-- name: ListChatAttentionDecisions :many
-- Open decision requests, addressed by the chain of the DM they were asked in.
SELECT d.id, s.chain_root_session_id, d.answer_revision, d.created_at
FROM decision_request d
JOIN chat_session s ON s.id = d.chat_session_id
WHERE d.human_user_id = $1 AND d.workspace_id = $2 AND d.status = 'open'
  AND s.chain_root_session_id IS NOT NULL;

-- name: ListChatAttentionLatestTurns :many
-- C2: the unclassified turn of each chain, if its latest recorded turn ended with no outcome (CHE-4149).
SELECT chain_root_session_id, task_id FROM (
  SELECT DISTINCT ON (o.chain_root_session_id) o.chain_root_session_id, o.task_id, o.kind
  FROM chat_turn_outcome o
  JOIN agent_task_queue t ON t.id = o.task_id
  JOIN chat_session s ON s.id = o.chat_session_id
  WHERE s.workspace_id = $1 AND o.user_id = $2
  ORDER BY o.chain_root_session_id, t.created_at DESC, t.id DESC
) latest
WHERE kind = 'unclassified';

-- name: ListChatAttentionMarks :many
SELECT subject_key, snoozed_until, acked_at FROM chat_attention_mark WHERE user_id = $1;

-- Write side of a DM send (CHE-4146). Both run in the message transaction.

-- name: ResolveChatAttentionOnHumanSend :exec
-- T2: a persisted send from Cheese settles "check later" and "unseen" for the whole chain.
UPDATE chat_attention_item
SET resolved_at = clock_timestamp(), resolved_by = 'human_send'
WHERE chain_root_session_id = $1 AND user_id = $2
  AND kind IN ('later', 'unseen') AND resolved_at IS NULL;

-- name: ListOpenChatAttentionAsksByID :many
-- The ids that are this user's open asks in this chain; a send may target only these.
SELECT id FROM chat_attention_item
WHERE id = ANY(@ids::uuid[]) AND chain_root_session_id = @chain_root_session_id AND user_id = @user_id
  AND kind = 'ask' AND resolved_at IS NULL;

-- name: ListOpenChatAttentionAsks :many
-- T2b single-ask fallback: every open ask of this user in this chain (the send waits only if there is one).
SELECT id FROM chat_attention_item
WHERE chain_root_session_id = @chain_root_session_id AND user_id = @user_id
  AND kind = 'ask' AND resolved_at IS NULL;

-- name: SetChatAttentionAsksWaiting :exec
-- T2b: the targeted asks wait on the task that consumes this send. They stay open.
UPDATE chat_attention_item
SET waiting_message_id = @message_id, waiting_task_id = @task_id
WHERE id = ANY(@ids::uuid[]) AND kind = 'ask' AND resolved_at IS NULL;

-- Agent outcomes and task end (CHE-4149). All run in one transaction per call.

-- name: ListChatAttentionAsksOfAgent :many
-- The agent's asks among ids (any state) in this chain for this user; outcomes may name only these.
SELECT id, (resolved_at IS NULL)::boolean AS open FROM chat_attention_item
WHERE id = ANY(@ids::uuid[]) AND chain_root_session_id = @chain_root_session_id AND user_id = @user_id
  AND agent_id = @agent_id AND kind = 'ask';

-- name: ListOpenChatAttentionAsksOfAgent :many
SELECT id FROM chat_attention_item
WHERE chain_root_session_id = @chain_root_session_id AND user_id = @user_id AND agent_id = @agent_id
  AND kind = 'ask' AND resolved_at IS NULL
ORDER BY opened_at, id;

-- name: GetChatAttentionAskOpenedByTask :one
SELECT id FROM chat_attention_item WHERE source_task_id = $1 AND kind = 'ask';

-- name: CreateChatAttentionAsk :one
-- T6 new: opened_at is the default clock_timestamp().
INSERT INTO chat_attention_item (workspace_id, chain_root_session_id, chat_session_id, user_id, agent_id, kind, source_task_id, last_outcome_task_id)
VALUES (@workspace_id, @chain_root_session_id, @chat_session_id, @user_id, @agent_id, 'ask', @source_task_id, @source_task_id)
RETURNING id;

-- name: ContinueChatAttentionAsks :exec
-- T6 continue: the ask returns to Cheese with its original opened_at. Waiting is cleared unless a
-- later task is the one it waits on. The watermark makes an older task or a repeat a no-op (T11);
-- tasks are ordered by (created_at, id).
UPDATE chat_attention_item i
SET last_outcome_task_id = @task_id,
    waiting_message_id = CASE WHEN i.waiting_task_id IS NULL OR i.waiting_task_id = @task_id THEN NULL ELSE i.waiting_message_id END,
    waiting_task_id = CASE WHEN i.waiting_task_id IS NULL OR i.waiting_task_id = @task_id THEN NULL ELSE i.waiting_task_id END
WHERE i.id = ANY(@ids::uuid[]) AND i.kind = 'ask' AND i.resolved_at IS NULL
  AND (i.last_outcome_task_id IS NULL OR (
        SELECT (o.created_at, o.id) FROM agent_task_queue o WHERE o.id = i.last_outcome_task_id
      ) < (SELECT (n.created_at, n.id) FROM agent_task_queue n WHERE n.id = @task_id));

-- name: ResolveChatAttentionAsksDone :exec
-- T7: only the named asks, under the same watermark.
UPDATE chat_attention_item i
SET resolved_at = clock_timestamp(), resolved_by = 'agent_done', last_outcome_task_id = @task_id,
    waiting_message_id = NULL, waiting_task_id = NULL
WHERE i.id = ANY(@ids::uuid[]) AND i.kind = 'ask' AND i.resolved_at IS NULL
  AND (i.last_outcome_task_id IS NULL OR (
        SELECT (o.created_at, o.id) FROM agent_task_queue o WHERE o.id = i.last_outcome_task_id
      ) < (SELECT (n.created_at, n.id) FROM agent_task_queue n WHERE n.id = @task_id));

-- name: RecordChatTurnOutcome :exec
-- One row per task; the first outcome wins.
INSERT INTO chat_turn_outcome (task_id, workspace_id, chat_session_id, chain_root_session_id, user_id, kind)
VALUES (@task_id, @workspace_id, @chat_session_id, @chain_root_session_id, @user_id, @kind)
ON CONFLICT (task_id) DO NOTHING;

-- name: ClearChatAttentionWaitingForTasks :exec
-- T10: whichever way a task ends, the asks that waited on it go back to Cheese.
UPDATE chat_attention_item
SET waiting_message_id = NULL, waiting_task_id = NULL
WHERE waiting_task_id = ANY(@task_ids::uuid[]);

-- name: RecordUnclassifiedChatTurns :exec
-- T9: a completed chat task that reported no outcome is an unclassified turn. Compaction tasks are not turns.
INSERT INTO chat_turn_outcome (task_id, workspace_id, chat_session_id, chain_root_session_id, user_id, kind)
SELECT t.id, s.workspace_id, s.id, s.chain_root_session_id, s.creator_id, 'unclassified'
FROM agent_task_queue t
JOIN chat_session s ON s.id = t.chat_session_id
WHERE t.id = ANY(@task_ids::uuid[]) AND t.status = 'completed' AND t.task_kind IS DISTINCT FROM 'compact'
  AND s.chain_root_session_id IS NOT NULL
ON CONFLICT (task_id) DO NOTHING;
