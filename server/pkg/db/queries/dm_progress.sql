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
-- Adds reasons to the follow of (issue, lineage). An active follow gains the
-- reasons and keeps its mute. An unfollowed row is a negative override: only an
-- explicit follow (origin or manual) reactivates it, with exactly the new
-- reasons, mute and baseline status; an inherited one returns no row. The
-- reporting root is set at creation and never changes (a trigger enforces it).
INSERT INTO dm_progress_follow (workspace_id, issue_id, lineage_id, owner_user_id, reasons, muted_at, last_rendered_status, reporting_root_id)
VALUES ($1, $2, $3, $4, sqlc.arg('reasons')::text[], sqlc.narg('muted_at'), $5, sqlc.arg('reporting_root_id'))
ON CONFLICT (issue_id, lineage_id) DO UPDATE
SET reasons = CASE
        WHEN dm_progress_follow.state = 'unfollowed' THEN EXCLUDED.reasons
        ELSE (SELECT COALESCE(array_agg(DISTINCT r ORDER BY r), '{}') FROM unnest(dm_progress_follow.reasons || EXCLUDED.reasons) AS r)
    END,
    muted_at = CASE WHEN dm_progress_follow.state = 'unfollowed' THEN EXCLUDED.muted_at ELSE dm_progress_follow.muted_at END,
    last_rendered_status = CASE WHEN dm_progress_follow.state = 'unfollowed' THEN EXCLUDED.last_rendered_status ELSE dm_progress_follow.last_rendered_status END,
    state = 'active'
WHERE dm_progress_follow.state = 'active' OR sqlc.arg('explicit')::boolean
RETURNING *;

-- name: IssueParentChainLoops :one
-- Whether the parent chain above an issue loops. The walk is iterative with no
-- depth limit. UNION (not UNION ALL) drops a row the walk has already produced,
-- so a repeated issue ends the walk through a hash lookup: linear in depth, no
-- per-row path array. A looping chain never reaches an issue without a parent.
WITH RECURSIVE walk AS (
    SELECT i.id, i.parent_issue_id FROM issue i WHERE i.id = $1
    UNION
    SELECT i.id, i.parent_issue_id
    FROM walk w JOIN issue i ON i.id = w.parent_issue_id
)
SELECT NOT EXISTS (SELECT 1 FROM walk WHERE walk.parent_issue_id IS NULL)::boolean AS loops;

-- name: InsertDMProgressDataIncident :exec
-- One incident per follow and kind, however often the child event repeats. The
-- unique index on (follow_id, kind) dedups without scanning the table.
INSERT INTO dm_progress_data_incident (workspace_id, issue_id, follow_id, kind)
VALUES ($1, $2, $3, $4)
ON CONFLICT (follow_id, kind) DO NOTHING;

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
-- Scope "issue": drops every reason of this issue's follow. The empty row stays
-- as the negative override.
UPDATE dm_progress_follow
SET state = 'unfollowed', reasons = '{}'
WHERE issue_id = $1 AND lineage_id = $2 AND state = 'active'
RETURNING *;

-- name: DropInheritedDMProgressReason :many
-- Scope "root": removes the reason 'inherited:<root>' from every follow of the
-- lane. A follow left with no reason is unfollowed; one with another reason
-- (a manual follow, another root) stays active.
UPDATE dm_progress_follow
SET reasons = array_remove(reasons, sqlc.arg('reason')::text),
    state = CASE WHEN cardinality(array_remove(reasons, sqlc.arg('reason')::text)) = 0 THEN 'unfollowed' ELSE state END
WHERE lineage_id = $1 AND state = 'active' AND sqlc.arg('reason')::text = ANY(reasons)
RETURNING *;

-- name: InsertDMProgressMovedUnderEvents :exec
-- CHE-3548: one "moved under" row per lane that actively follows the moved
-- issue. Follows are not touched; muted lanes still get the row.
INSERT INTO dm_progress_lane_event (workspace_id, lineage_id, issue_id, kind, related_issue_id)
SELECT f.workspace_id, f.lineage_id, f.issue_id, 'moved_under', sqlc.arg('new_parent_id')::uuid
FROM dm_progress_follow AS f
WHERE f.issue_id = $1 AND f.workspace_id = $2 AND f.state = 'active';

-- name: DeleteDMProgressForIssue :exec
-- No FKs (repo rule): issue delete sweeps its follows, lane events and pending rows here.
WITH gone AS (
    DELETE FROM dm_progress_lane_event AS e WHERE e.issue_id = $1 AND e.workspace_id = $2
), gone_pending AS (
    DELETE FROM dm_progress_pending AS p WHERE p.issue_id = $1 AND p.workspace_id = $2
)
DELETE FROM dm_progress_follow AS f WHERE f.issue_id = $1 AND f.workspace_id = $2;

-- name: DeleteDMProgressForLineage :exec
-- No FKs (repo rule): chat session delete sweeps its follows, lane events, pending rows and lane here.
-- Callers hold the chat_session lock, so a new follow cannot slip in between.
WITH gone AS (
    DELETE FROM dm_progress_follow AS f WHERE f.lineage_id = $1 AND f.workspace_id = $2
), gone_events AS (
    DELETE FROM dm_progress_lane_event AS e WHERE e.lineage_id = $1 AND e.workspace_id = $2
), gone_pending AS (
    DELETE FROM dm_progress_pending AS p WHERE p.lineage_id = $1 AND p.workspace_id = $2
)
DELETE FROM dm_progress_lane AS l WHERE l.lineage_id = $1 AND l.workspace_id = $2;

-- name: DeleteDMProgressByWorkspace :exec
-- No FKs (repo rule): workspace delete sweeps all five tables here.
WITH gone_follows AS (
    DELETE FROM dm_progress_follow AS f WHERE f.workspace_id = $1
), gone_events AS (
    DELETE FROM dm_progress_lane_event AS e WHERE e.workspace_id = $1
), gone_pending AS (
    DELETE FROM dm_progress_pending AS p WHERE p.workspace_id = $1
), gone_incidents AS (
    DELETE FROM dm_progress_data_incident AS i WHERE i.workspace_id = $1
)
DELETE FROM dm_progress_lane AS l WHERE l.workspace_id = $1;
