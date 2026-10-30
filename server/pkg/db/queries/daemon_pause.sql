-- name: UpsertDaemonPause :one
INSERT INTO daemon_pause (workspace_id, daemon_id, reason, paused_by)
VALUES ($1, $2, $3, $4)
ON CONFLICT (workspace_id, daemon_id) DO UPDATE
SET reason = EXCLUDED.reason, paused_by = EXCLUDED.paused_by, paused_at = now()
RETURNING *;

-- name: DeleteDaemonPause :execrows
DELETE FROM daemon_pause
WHERE workspace_id = $1 AND daemon_id = $2;

-- name: ListDaemonPausesByWorkspace :many
-- Live pauses only: a pause past its auto-expiry no longer holds the machine.
SELECT * FROM daemon_pause
WHERE workspace_id = @workspace_id
  AND paused_at + make_interval(secs => CASE reason WHEN 'maintenance' THEN @maintenance_secs::int ELSE @other_secs::int END) > now();

-- name: ListPausedDaemons :many
-- Claim gate: which of the given "<workspace_id>|<daemon_id>" keys are paused.
-- A batch claim can span workspaces; the table holds one row per paused machine.
SELECT workspace_id, daemon_id FROM daemon_pause
WHERE workspace_id::text || '|' || daemon_id = ANY(@keys::text[])
  AND paused_at + make_interval(secs => CASE reason WHEN 'maintenance' THEN @maintenance_secs::int ELSE @other_secs::int END) > now();

-- name: CountRunningTasksByDaemon :many
-- Tasks still occupying a machine: handed to the daemon or running. Queued
-- tasks do not count; they wait on the server while the daemon is paused.
SELECT rt.daemon_id::text AS daemon_id, COUNT(*)::int AS running_tasks
FROM agent_task_queue t
JOIN agent_runtime rt ON rt.id = t.runtime_id
WHERE rt.workspace_id = $1
  AND rt.daemon_id IS NOT NULL
  AND t.status IN ('dispatched', 'running', 'waiting_local_directory')
GROUP BY rt.daemon_id;
