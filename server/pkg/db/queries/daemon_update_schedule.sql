-- name: GetDaemonUpdateSchedule :one
SELECT * FROM daemon_update_schedule
WHERE machine_owner_id = $1 AND daemon_id = $2;

-- name: UpsertDaemonUpdateSchedule :one
INSERT INTO daemon_update_schedule (machine_owner_id, daemon_id, enabled, cron, timezone, window_minutes, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (machine_owner_id, daemon_id) DO UPDATE
SET enabled = EXCLUDED.enabled, cron = EXCLUDED.cron, timezone = EXCLUDED.timezone,
    window_minutes = EXCLUDED.window_minutes, updated_by = EXCLUDED.updated_by, updated_at = now()
RETURNING *;

-- name: DeleteDaemonUpdateSchedule :execrows
DELETE FROM daemon_update_schedule
WHERE machine_owner_id = $1 AND daemon_id = $2;

-- name: ListDaemonUpdateSchedules :many
-- Schedules among the given owners and daemon_ids: a superset of the pairs
-- asked for; callers match the exact pair.
SELECT * FROM daemon_update_schedule
WHERE machine_owner_id = ANY(@machine_owner_ids::uuid[])
  AND daemon_id = ANY(@daemon_ids::text[]);
