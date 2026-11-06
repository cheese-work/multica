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
-- C2: the latest finished chat task of each chain. Untyped until L5 records outcomes.
SELECT DISTINCT ON (s.chain_root_session_id) s.chain_root_session_id, t.id AS task_id
FROM agent_task_queue t
JOIN chat_session s ON s.id = t.chat_session_id
WHERE s.workspace_id = $1 AND s.creator_id = $2 AND t.status = 'completed'
  AND s.chain_root_session_id IS NOT NULL
ORDER BY s.chain_root_session_id, t.created_at DESC, t.id DESC;

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
