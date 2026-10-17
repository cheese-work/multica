-- name: ListInboxItems :many
SELECT i.*,
       iss.status AS issue_status,
       iss.priority AS issue_priority
FROM inbox_item i
LEFT JOIN issue iss ON iss.id = i.issue_id
WHERE i.workspace_id = $1 AND i.recipient_type = $2 AND i.recipient_id = $3 AND i.archived = false
  -- DM records vanish once the recipient can no longer read the DM (CHE-1793).
  AND inbox_item_visible(i.type, i.chat_session_id, i.workspace_id, i.recipient_id)
-- A coalesced DM record moves to the top when a new reply updates it.
ORDER BY COALESCE(i.updated_at, i.created_at) DESC;

-- name: ListArchivedInboxItems :many
-- Archived counterpart of ListInboxItems, backing the inbox's "Archived"
-- sub-view (MUL-3736).
--
-- An issue whose group still has an active row is excluded: archiving is
-- issue-level, so a NEW notification on an already-archived issue leaves the
-- old archived rows in place alongside the fresh active one. The issue belongs
-- in the main inbox at that point, and the two lists must stay mutually
-- exclusive per issue group — otherwise the same issue renders in both. The
-- exclusion lives here rather than in the client so neither list depends on
-- the other's cache being loaded. Items without an issue_id group on their own
-- id and can never have an active sibling, hence the IS NULL short-circuit.
--
-- LIMIT applies to ISSUE GROUPS, not raw notification rows. Applying it after
-- the final SELECT lets one noisy issue consume all 200 rows and hide other
-- archived issues, which makes both the visible list and its count too small.
-- The response stays bounded at two rows per group: the newest row the UI
-- renders plus, when different, the newest row carrying a comment anchor. The
-- client already merges those two rows while deduplicating, preserving direct
-- comment landing without sending every historical notification in the group.
-- Keep the materialized working set narrow: full row data is joined only for
-- the final selected ids, not copied for every archived notification scanned.
WITH eligible_archived AS MATERIALIZED (
    SELECT i.id,
           COALESCE(i.issue_id, i.id) AS group_id,
           i.created_at,
           i.details
    FROM inbox_item i
    WHERE i.workspace_id = $1
      AND i.recipient_type = $2
      AND i.recipient_id = $3
      AND i.archived = true
      AND inbox_item_visible(i.type, i.chat_session_id, i.workspace_id, i.recipient_id)
      AND (i.issue_id IS NULL OR NOT EXISTS (
          SELECT 1
          FROM inbox_item active
          WHERE active.workspace_id = i.workspace_id
            AND active.recipient_type = i.recipient_type
            AND active.recipient_id = i.recipient_id
            AND active.issue_id = i.issue_id
            AND active.archived = false
      ))
), newest_groups AS (
    SELECT DISTINCT ON (group_id)
           group_id,
           id AS newest_id,
           created_at AS newest_created_at
    FROM eligible_archived
    ORDER BY group_id, created_at DESC, id DESC
), limited_groups AS (
    SELECT group_id, newest_id
    FROM newest_groups
    ORDER BY newest_created_at DESC, newest_id DESC
    LIMIT 200
), comment_anchors AS (
    SELECT DISTINCT ON (archived.group_id)
           archived.group_id,
           archived.id
    FROM eligible_archived archived
    JOIN limited_groups selected USING (group_id)
    WHERE NULLIF(archived.details->>'comment_id', '') IS NOT NULL
    ORDER BY archived.group_id, archived.created_at DESC, archived.id DESC
), selected_ids AS (
    SELECT newest_id AS id FROM limited_groups
    UNION
    SELECT id FROM comment_anchors
)
SELECT i.*,
       iss.status AS issue_status,
       iss.priority AS issue_priority
FROM inbox_item i
JOIN selected_ids selected ON selected.id = i.id
LEFT JOIN issue iss ON iss.id = i.issue_id
ORDER BY i.created_at DESC, i.id DESC;

-- name: GetInboxItem :one
SELECT * FROM inbox_item
WHERE id = $1;

-- name: GetInboxItemInWorkspace :one
SELECT * FROM inbox_item
WHERE id = $1 AND workspace_id = $2;

-- name: CreateInboxItem :one
INSERT INTO inbox_item (
    workspace_id, recipient_type, recipient_id,
    type, severity, issue_id, title, body,
    actor_type, actor_id, details, id,
    chat_session_id, chat_message_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, COALESCE(sqlc.narg('id')::uuid, gen_random_uuid()),
    sqlc.narg('chat_session_id')::uuid, sqlc.narg('chat_message_id')::uuid)
RETURNING *;

-- name: MarkInboxRead :one
UPDATE inbox_item SET read = true
WHERE id = $1
RETURNING *;

-- name: MarkInboxUnread :one
-- Exact inverse of MarkInboxRead, and item-level for the same reason it is:
-- the inbox renders one row per issue carrying that group's NEWEST item, and
-- the group's read state is that item's read state. Flipping the whole group
-- unread would resurrect older siblings the user already dealt with and
-- inflate CountUnreadInbox (which counts raw rows), while changing nothing the
-- UI shows.
UPDATE inbox_item SET read = false
WHERE id = $1
RETURNING *;

-- name: ArchiveInboxItem :one
UPDATE inbox_item SET archived = true
WHERE id = $1
RETURNING *;

-- name: ArchiveInboxByIssue :execrows
UPDATE inbox_item SET archived = true
WHERE workspace_id = $1 AND recipient_type = $2 AND recipient_id = $3 AND issue_id = $4 AND archived = false;

-- name: UnarchiveInboxItem :one
-- Deliberately does not touch `read`: unarchiving restores an item to the main
-- inbox in the exact read/unread state it was archived in, so restoring an
-- unread item legitimately raises the unread badge again (MUL-3736).
UPDATE inbox_item SET archived = false
WHERE id = $1
RETURNING *;

-- name: UnarchiveInboxByIssue :execrows
-- Issue-level restore, mirroring ArchiveInboxByIssue: archiving one item
-- archives every sibling for the same issue, so unarchiving must bring the
-- whole group back. Leaves `read` untouched for the same reason as above.
UPDATE inbox_item SET archived = false
WHERE workspace_id = $1 AND recipient_type = $2 AND recipient_id = $3 AND issue_id = $4 AND archived = true;

-- name: ArchiveInboxByIssueAndType :many
UPDATE inbox_item SET archived = true
WHERE workspace_id = $1 AND issue_id = $2 AND type = $3 AND archived = false
RETURNING recipient_type, recipient_id;

-- name: CountUnreadInbox :one
SELECT count(*) FROM inbox_item
WHERE workspace_id = $1 AND recipient_type = $2 AND recipient_id = $3 AND read = false AND archived = false
  AND inbox_item_visible(type, chat_session_id, workspace_id, recipient_id);

-- name: CountUnreadInboxByWorkspace :many
-- Per-workspace unread inbox counts for a recipient member, matching the
-- inbox UI's deduplicated view: notifications are grouped per issue
-- (Linear-style, one row per issue) and an issue counts as unread only when
-- its NEWEST non-archived item is unread. Opening an issue marks just that
-- newest item read, so counting raw unread rows would keep older siblings
-- alive and light the switcher dot for a workspace whose inbox the user sees
-- as empty (MUL-3695). Items without an issue group on their own id. The
-- member join keeps counts scoped to workspaces the user still belongs to,
-- so a stale item left behind in a workspace the user has since left cannot
-- light the dot.
SELECT newest.workspace_id, count(*) AS count
FROM (
    SELECT DISTINCT ON (i.workspace_id, COALESCE(i.issue_id, i.id))
        i.workspace_id, i.read
    FROM inbox_item i
    JOIN member m ON m.workspace_id = i.workspace_id AND m.user_id = i.recipient_id
    WHERE i.recipient_type = 'member'
      AND i.recipient_id = $1
      AND i.archived = false
      AND inbox_item_visible(i.type, i.chat_session_id, i.workspace_id, i.recipient_id)
    ORDER BY i.workspace_id, COALESCE(i.issue_id, i.id), i.created_at DESC
) newest
WHERE newest.read = false
GROUP BY newest.workspace_id;

-- name: MarkAllInboxRead :one
-- Reading a DM reply record reads the DM (CHE-1793), so the cursor of every DM
-- whose record flips here advances in the same statement.
WITH marked AS (
    UPDATE inbox_item i SET read = true
    WHERE i.workspace_id = $1 AND i.recipient_type = 'member' AND i.recipient_id = $2 AND i.archived = false AND i.read = false
    RETURNING i.type, i.chat_session_id, i.chat_message_id
), advanced AS (
    UPDATE chat_session cs SET last_read_at = GREATEST(cs.last_read_at, m.created_at)
    FROM marked
    JOIN chat_message m ON m.id = marked.chat_message_id
    WHERE marked.type = 'dm_reply' AND cs.id = marked.chat_session_id AND cs.creator_id = $2
    RETURNING cs.id
)
SELECT count(*)::bigint FROM marked;

-- name: ArchiveAllInbox :execrows
UPDATE inbox_item SET archived = true
WHERE workspace_id = $1 AND recipient_type = 'member' AND recipient_id = $2 AND archived = false;

-- name: ArchiveAllReadInbox :execrows
-- "Read" is the state of the one issue row the inbox renders: the newest
-- active notification in that issue group. Archive every row in groups whose
-- newest row is read, and leave an unread group wholly untouched. Updating raw
-- read rows instead makes an older unread sibling reappear after the newest
-- row is archived (and can archive an older read sibling under an unread row).
WITH newest_groups AS (
    SELECT DISTINCT ON (COALESCE(i.issue_id, i.id))
           COALESCE(i.issue_id, i.id) AS group_id,
           i.read
    FROM inbox_item i
    WHERE i.workspace_id = $1
      AND i.recipient_type = 'member'
      AND i.recipient_id = $2
      AND i.archived = false
    ORDER BY COALESCE(i.issue_id, i.id), i.created_at DESC, i.id DESC
), read_groups AS (
    SELECT group_id
    FROM newest_groups
    WHERE read = true
)
UPDATE inbox_item i SET archived = true
FROM read_groups selected
WHERE i.workspace_id = $1
  AND i.recipient_type = 'member'
  AND i.recipient_id = $2
  AND i.archived = false
  AND COALESCE(i.issue_id, i.id) = selected.group_id;

-- name: ArchiveCompletedInbox :execrows
UPDATE inbox_item i SET archived = true
WHERE i.workspace_id = $1 AND i.recipient_type = 'member' AND i.recipient_id = $2 AND i.archived = false
  AND i.issue_id IN (
    SELECT id FROM issue
    WHERE workspace_id = $1
      AND status = ANY(sqlc.arg('terminal_status_keys')::text[])
  );

-- name: UpsertDMReplyInboxItem :one
-- One unread record per (recipient, DM): the partial unique index on
-- (recipient_id, dedupe_key) WHERE unread turns a later reply into an update
-- that bumps reply_count, moves the row to the top and revives it from the
-- archive. Once the record is read the next reply inserts a fresh one. The body
-- stays NULL on purpose: a notification never carries message text.
INSERT INTO inbox_item (
    id, workspace_id, recipient_type, recipient_id, type, severity, title,
    actor_type, actor_id, details, chat_session_id, chat_message_id, dedupe_key, updated_at
) VALUES (
    @id, @workspace_id, 'member', @recipient_id, 'dm_reply', 'info', @title,
    'agent', @agent_id, '{"reply_count": 1}'::jsonb, @chat_session_id, @chat_message_id, @dedupe_key, now()
)
ON CONFLICT (recipient_id, dedupe_key) WHERE dedupe_key IS NOT NULL AND read = false
DO UPDATE SET
    title = EXCLUDED.title,
    actor_id = EXCLUDED.actor_id,
    chat_message_id = EXCLUDED.chat_message_id,
    archived = false,
    updated_at = now(),
    details = jsonb_set(
        inbox_item.details, '{reply_count}',
        to_jsonb(COALESCE((inbox_item.details->>'reply_count')::int, 1) + 1))
RETURNING *;

-- name: MarkDMRepliesRead :many
-- Reads every unread dm_reply record of one DM for its creator. Decision
-- records linked to the same DM are left alone; they close when the decision
-- is resolved.
UPDATE inbox_item SET read = true
WHERE chat_session_id = $1 AND recipient_id = $2 AND type = 'dm_reply' AND read = false
RETURNING *;

-- name: MarkInboxReadByDecisionRequest :many
UPDATE inbox_item SET read = true
WHERE workspace_id = $1 AND details->>'decision_request_id' = sqlc.arg('decision_request_id')::text AND read = false
RETURNING *;

-- name: AdvanceChatSessionReadTo :exec
-- Moves the DM read cursor up to a message (never back), so marking its inbox
-- record read leaves the DM with no unread replies up to that message.
UPDATE chat_session cs SET last_read_at = GREATEST(cs.last_read_at, m.created_at)
FROM chat_message m
WHERE cs.id = sqlc.arg('session_id') AND m.id = sqlc.arg('message_id') AND m.chat_session_id = cs.id;

-- name: GetDMChannelSwitches :one
SELECT dm_channel_enabled(@workspace_id::uuid, @user_id::uuid, 'in_app') AS in_app,
       dm_channel_enabled(@workspace_id::uuid, @user_id::uuid, 'push') AS push;

-- name: ChatSessionHasCurrentChannelBinding :one
SELECT EXISTS (
    SELECT 1 FROM channel_chat_session_binding
    WHERE chat_session_id = $1 AND retired_at IS NULL
) AS bound;
