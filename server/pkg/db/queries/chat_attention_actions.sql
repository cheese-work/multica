-- Write side of DM attention (CHE-4147, plan r4 T3-T5). Callers pass the exact displayed
-- subject keys; ownership is checked by comparing the rows returned with the ids sent.

-- name: ListOwnedAttentionItems :many
SELECT id, chain_root_session_id, kind, resolved_at
FROM chat_attention_item
WHERE id = ANY(@ids::uuid[]) AND user_id = @user_id AND workspace_id = @workspace_id;

-- name: ListOwnedAttentionDecisions :many
SELECT d.id, s.chain_root_session_id
FROM decision_request d
JOIN chat_session s ON s.id = d.chat_session_id
WHERE d.id = ANY(@ids::uuid[]) AND d.human_user_id = @user_id AND d.workspace_id = @workspace_id
  AND s.chain_root_session_id IS NOT NULL;

-- name: ListOwnedAttentionTurns :many
SELECT t.id, s.chain_root_session_id
FROM agent_task_queue t
JOIN chat_session s ON s.id = t.chat_session_id
WHERE t.id = ANY(@ids::uuid[]) AND s.creator_id = @user_id AND s.workspace_id = @workspace_id
  AND s.chain_root_session_id IS NOT NULL;

-- name: ListAttentionChainAgents :many
-- The agent each of the user's DMs talks to (the canonical session of its chain), for the private-agent gate.
SELECT chain_root_session_id AS root_id, agent_id
FROM chat_session
WHERE workspace_id = @workspace_id AND creator_id = @user_id AND continued_into_session_id IS NULL
  AND chain_root_session_id IS NOT NULL;

-- name: LockAttentionChain :exec
-- Serialises writers of one chain's attention rows (open later, unseen) for the transaction.
SELECT pg_advisory_xact_lock(hashtextextended(@chain_root::text, 0));

-- name: ResolveAttentionItemsXong :many
-- Only still-open rows: a repeat or a row closed since the snapshot is left alone.
UPDATE chat_attention_item
SET resolved_at = clock_timestamp(), resolved_by = 'xong', resolve_batch_id = @batch_id
WHERE id = ANY(@ids::uuid[]) AND user_id = @user_id AND resolved_at IS NULL
RETURNING id;

-- name: AckAttentionTurns :exec
-- First acknowledgement wins, so a repeat keeps its original batch.
INSERT INTO chat_attention_mark (user_id, subject_key, chain_root_session_id, acked_at, ack_batch_id)
SELECT @user_id, k.subject_key, r.chain_root_session_id, clock_timestamp(), @batch_id
FROM unnest(@subject_keys::text[]) WITH ORDINALITY AS k(subject_key, n)
JOIN unnest(@chain_roots::uuid[]) WITH ORDINALITY AS r(chain_root_session_id, n) ON r.n = k.n
ON CONFLICT (user_id, subject_key) DO UPDATE
SET acked_at = clock_timestamp(), ack_batch_id = EXCLUDED.ack_batch_id
WHERE chat_attention_mark.acked_at IS NULL;

-- name: ReopenXongBatchItems :execrows
-- Undo: only rows this batch closed and nobody changed since. A later item stays closed
-- when the chain has since opened another one.
UPDATE chat_attention_item i
SET resolved_at = NULL, resolved_by = NULL, resolve_batch_id = NULL
WHERE i.resolve_batch_id = @batch_id AND i.user_id = @user_id AND i.workspace_id = @workspace_id
  AND i.resolved_by = 'xong'
  AND NOT (i.chain_root_session_id = ANY(@hidden_roots::uuid[]))
  AND NOT (i.kind = 'later' AND EXISTS (
    SELECT 1 FROM chat_attention_item o
    WHERE o.chain_root_session_id = i.chain_root_session_id AND o.user_id = i.user_id
      AND o.kind = 'later' AND o.resolved_at IS NULL));

-- name: ClearAttentionAckBatch :execrows
UPDATE chat_attention_mark m SET acked_at = NULL, ack_batch_id = NULL
WHERE m.user_id = @user_id AND m.ack_batch_id = @batch_id
  AND m.chain_root_session_id IN (SELECT id FROM chat_session WHERE workspace_id = @workspace_id)
  AND NOT (m.chain_root_session_id = ANY(@hidden_roots::uuid[]));

-- name: AttentionItemExists :one
SELECT EXISTS (SELECT 1 FROM chat_attention_item WHERE id = @id) AS item_exists;

-- name: ReplaceOpenLaterAndUnseen :exec
-- The open later is always replaced. An unseen item is replaced only when the request named it:
-- one marked after the displayed snapshot is a new obligation and stays open.
UPDATE chat_attention_item
SET resolved_at = clock_timestamp(), resolved_by = 'replaced'
WHERE chain_root_session_id = @chain_root AND user_id = @user_id AND resolved_at IS NULL
  AND (kind = 'later' OR (kind = 'unseen' AND id = ANY(@sent_item_ids::uuid[])));

-- name: InsertLaterAttentionItem :exec
INSERT INTO chat_attention_item (id, workspace_id, chain_root_session_id, chat_session_id, user_id, kind, due_at)
VALUES (@id, @workspace_id, @chain_root, @chat_session_id, @user_id, 'later', @due_at);

-- name: SnoozeAttentionSubjects :exec
-- Presentation only; a decision key is snoozed here but is never resolved by Để sau.
INSERT INTO chat_attention_mark (user_id, subject_key, chain_root_session_id, snoozed_until)
SELECT @user_id, k.subject_key, r.chain_root_session_id, @snoozed_until
FROM unnest(@subject_keys::text[]) WITH ORDINALITY AS k(subject_key, n)
JOIN unnest(@chain_roots::uuid[]) WITH ORDINALITY AS r(chain_root_session_id, n) ON r.n = k.n
ON CONFLICT (user_id, subject_key) DO UPDATE SET snoozed_until = EXCLUDED.snoozed_until;

-- name: OpenUnseenAttentionItem :exec
-- Explicit mark-unseen. No-op, like the cursor move, when no assistant message counts;
-- one open unseen item per chain and user.
INSERT INTO chat_attention_item (workspace_id, chain_root_session_id, chat_session_id, user_id, kind)
SELECT @workspace_id, @chain_root, @chat_session_id, @user_id, 'unseen'
WHERE EXISTS (
    SELECT 1 FROM chat_message m
    WHERE m.chat_session_id = @chat_session_id AND m.role = 'assistant'
      AND m.message_kind NOT IN ('decision_cancel_notice', 'decision_reference'))
  AND NOT EXISTS (
    SELECT 1 FROM chat_attention_item o
    WHERE o.chain_root_session_id = @chain_root AND o.user_id = @user_id
      AND o.kind = 'unseen' AND o.resolved_at IS NULL);
