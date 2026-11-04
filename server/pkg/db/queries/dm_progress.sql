-- name: ResolveChatLineageRoot :one
-- Root of a DM's continuation chain: follow continued_from_session_id back to
-- the first session. The depth bound makes a cyclic chain terminate. Only a
-- session with no predecessor is a root: a cyclic or over-deep chain returns no
-- row instead of a partial root.
WITH RECURSIVE chain AS (
    SELECT chat_session.id, chat_session.continued_from_session_id AS parent, 1 AS depth
    FROM chat_session WHERE chat_session.id = $1
    UNION ALL
    SELECT s.id, s.continued_from_session_id, c.depth + 1
    FROM chat_session s JOIN chain c ON s.id = c.parent
    WHERE c.depth < 32
)
SELECT chain.id FROM chain WHERE chain.parent IS NULL;

-- name: GetNativeChatSession :one
-- A DM created in the product: channel-bridged chats (Lark, Telegram) are out.
SELECT session.* FROM chat_session session
WHERE session.id = $1
  AND session.channel_origin_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM channel_chat_session_binding binding WHERE binding.chat_session_id = session.id);

-- name: EnsureDMProgressLane :exec
INSERT INTO dm_progress_lane (lineage_id, workspace_id, owner_user_id)
VALUES ($1, $2, $3)
ON CONFLICT (lineage_id) DO NOTHING;

-- name: LockDMProgressLane :one
SELECT * FROM dm_progress_lane WHERE lineage_id = $1 FOR UPDATE;

-- name: InsertDMProgressFollow :one
-- No row comes back when an active follow already exists. An unfollowed one is
-- reactivated with the new source, mute and baseline status; its reporting root
-- is immutable and stays.
INSERT INTO dm_progress_follow (workspace_id, issue_id, lineage_id, owner_user_id, source, muted_at, last_rendered_status, parent_follow_id, reporting_root_id)
VALUES ($1, $2, $3, $4, $5, sqlc.narg('muted_at'), $6, sqlc.narg('parent_follow_id'), $7)
ON CONFLICT (issue_id, lineage_id) DO UPDATE
SET state = 'active', source = EXCLUDED.source, muted_at = EXCLUDED.muted_at,
    last_rendered_status = EXCLUDED.last_rendered_status, parent_follow_id = EXCLUDED.parent_follow_id
WHERE dm_progress_follow.state = 'unfollowed'
RETURNING *;

-- name: ResolveDMProgressReportingRoot :one
-- Stored reporting root of a parent follow, provided its parent chain ends. The
-- chain walk is iterative with no depth limit; the CYCLE clause stops it at a
-- repeated follow, so a corrupt chain terminates. A looping chain never reaches
-- a parentless follow: no row comes back for it. The root is read from the
-- parent, not from the chain's current top, so a reparent never splits a rollup.
WITH RECURSIVE walk AS (
    SELECT f.id, f.parent_follow_id FROM dm_progress_follow f WHERE f.id = $1
    UNION ALL
    SELECT f.id, f.parent_follow_id
    FROM walk w JOIN dm_progress_follow f ON f.id = w.parent_follow_id
) CYCLE id SET is_cycle USING path
SELECT p.reporting_root_id FROM dm_progress_follow p
WHERE p.id = $1 AND EXISTS (SELECT 1 FROM walk WHERE walk.parent_follow_id IS NULL);

-- name: InsertDMProgressDataIncident :exec
INSERT INTO dm_progress_data_incident (workspace_id, issue_id, follow_id, kind)
VALUES ($1, $2, $3, $4);

-- name: CountDMProgressDescendants :one
-- Tracked descendants of a reporting root in one lane. (issue_id, lineage_id) is
-- unique, so each issue counts once; the root itself is not its own descendant.
SELECT count(*) FROM dm_progress_follow
WHERE lineage_id = $1 AND reporting_root_id = $2 AND state = 'active' AND issue_id <> $2;

-- name: ListDMProgressDescendants :many
-- Keyset pages by issue_id; nothing is truncated, the caller walks every page.
SELECT issue_id FROM dm_progress_follow
WHERE lineage_id = $1 AND reporting_root_id = $2 AND state = 'active' AND issue_id <> $2
  AND (sqlc.narg('after_issue_id')::uuid IS NULL OR issue_id > sqlc.narg('after_issue_id')::uuid)
ORDER BY issue_id
LIMIT $3;

-- name: ListActiveDMProgressFollowsForIssue :many
-- Lineage order, so a caller locking each lane in turn always locks in one order.
SELECT * FROM dm_progress_follow
WHERE issue_id = $1 AND state = 'active'
ORDER BY lineage_id;

-- name: GetActiveDMProgressFollow :one
SELECT * FROM dm_progress_follow WHERE issue_id = $1 AND lineage_id = $2 AND state = 'active';

-- name: ListDMProgressFollowsByLineage :many
SELECT * FROM dm_progress_follow
WHERE lineage_id = $1 AND state = 'active'
ORDER BY created_at, id;

-- name: SetDMProgressFollowMuted :one
UPDATE dm_progress_follow
SET muted_at = CASE WHEN sqlc.arg('muted')::boolean THEN COALESCE(muted_at, now()) ELSE NULL END
WHERE issue_id = $1 AND lineage_id = $2 AND state = 'active'
RETURNING *;

-- name: UnfollowDMProgressFollow :one
UPDATE dm_progress_follow
SET state = 'unfollowed'
WHERE issue_id = $1 AND lineage_id = $2 AND state = 'active'
RETURNING *;

-- name: DeleteDMProgressForIssue :exec
-- No FKs (repo rule): issue delete sweeps its follows here.
DELETE FROM dm_progress_follow WHERE issue_id = $1 AND workspace_id = $2;

-- name: DeleteDMProgressForLineage :exec
-- No FKs (repo rule): chat session delete sweeps its follows and lane here.
-- Callers hold the chat_session lock, so a new follow cannot slip in between.
WITH gone AS (
    DELETE FROM dm_progress_follow AS f WHERE f.lineage_id = $1 AND f.workspace_id = $2
)
DELETE FROM dm_progress_lane AS l WHERE l.lineage_id = $1 AND l.workspace_id = $2;

-- name: DeleteDMProgressByWorkspace :exec
-- No FKs (repo rule): workspace delete sweeps all three tables here.
WITH gone_follows AS (
    DELETE FROM dm_progress_follow AS f WHERE f.workspace_id = $1
), gone_incidents AS (
    DELETE FROM dm_progress_data_incident AS i WHERE i.workspace_id = $1
)
DELETE FROM dm_progress_lane AS l WHERE l.workspace_id = $1;
