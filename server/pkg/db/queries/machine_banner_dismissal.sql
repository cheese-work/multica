-- name: ListMachineBannerDismissals :many
-- The caller's stored dismissals in one workspace (CHE-9190).
SELECT machine_owner_id, daemon_id, event_id
FROM machine_banner_dismissal
WHERE workspace_id = $1 AND user_id = $2;

-- name: LockMachineBannerDismissal :exec
-- Serializes the dismiss and reopen writes of one member on one machine, so a
-- write validated against an older offline period cannot land after a newer
-- one. Same pattern as LockCommentThread.
SELECT pg_advisory_xact_lock(hashtextextended(
    sqlc.arg(workspace_id)::uuid::text || ':' || sqlc.arg(user_id)::uuid::text || ':' ||
    sqlc.arg(machine_owner_id)::uuid::text || ':' || sqlc.arg(daemon_id)::text || ':banner_dismissal', 0));

-- name: UpsertMachineBannerDismissal :exec
INSERT INTO machine_banner_dismissal (workspace_id, user_id, machine_owner_id, daemon_id, event_id)
VALUES (@workspace_id, @user_id, @machine_owner_id, @daemon_id, @event_id)
ON CONFLICT (workspace_id, user_id, machine_owner_id, daemon_id)
DO UPDATE SET event_id = EXCLUDED.event_id, dismissed_at = now();

-- name: DeleteMachineBannerDismissal :exec
-- Reopen deletes only the period it names, so a late reopen of an older period
-- leaves a newer dismissal alone.
DELETE FROM machine_banner_dismissal
WHERE workspace_id = @workspace_id AND user_id = @user_id
  AND machine_owner_id = @machine_owner_id AND daemon_id = @daemon_id
  AND event_id = @event_id;

-- name: DeleteMachineBannerDismissalsByMember :exec
-- Member revoke: the departed member's dismissals in this workspace only.
DELETE FROM machine_banner_dismissal
WHERE workspace_id = @workspace_id AND user_id = @user_id;
