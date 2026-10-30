-- name: UpsertDaemonPause :one
INSERT INTO daemon_pause (workspace_id, daemon_id, reason, paused_by)
VALUES ($1, $2, $3, $4)
ON CONFLICT (workspace_id, daemon_id) DO UPDATE
SET reason = EXCLUDED.reason, paused_by = EXCLUDED.paused_by, paused_at = now()
RETURNING *;

-- name: DeleteDaemonPause :execrows
-- A daemon_id is one machine, so resume lifts the pause in every workspace.
DELETE FROM daemon_pause
WHERE daemon_id = $1;

-- name: ListLiveDaemonPauses :many
-- Live pauses for the given machines, whichever workspace set them: one daemon
-- polls every workspace its owner joined, so a pause holds the whole machine.
-- A pause past its auto-expiry no longer holds it.
SELECT DISTINCT ON (daemon_id) * FROM daemon_pause
WHERE daemon_id = ANY(@daemon_ids::text[])
  AND paused_at + make_interval(secs => CASE reason WHEN 'maintenance' THEN @maintenance_secs::int ELSE @other_secs::int END) > now()
ORDER BY daemon_id, paused_at DESC;

-- name: CountRunningTasksByDaemon :many
-- Tasks still occupying a machine, in any workspace: handed to the daemon or
-- running. Queued tasks do not count; they wait on the server while the daemon
-- is paused.
SELECT rt.daemon_id::text AS daemon_id, COUNT(*)::int AS running_tasks
FROM agent_task_queue t
JOIN agent_runtime rt ON rt.id = t.runtime_id
WHERE rt.daemon_id = ANY(@daemon_ids::text[])
  AND t.status IN ('dispatched', 'running', 'waiting_local_directory')
GROUP BY rt.daemon_id;
