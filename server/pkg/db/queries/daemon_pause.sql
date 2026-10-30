-- name: UpsertDaemonPause :one
-- One row per machine: a second pause, from any workspace the machine serves,
-- replaces the reason, actor and clock, so the gate and expires_at share a row.
INSERT INTO daemon_pause (machine_owner_id, daemon_id, workspace_id, reason, paused_by)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (machine_owner_id, daemon_id) DO UPDATE
SET workspace_id = EXCLUDED.workspace_id, reason = EXCLUDED.reason,
    paused_by = EXCLUDED.paused_by, paused_at = now()
RETURNING *;

-- name: DeleteDaemonPause :execrows
DELETE FROM daemon_pause
WHERE machine_owner_id = $1 AND daemon_id = $2;

-- name: ListLiveDaemonPauses :many
-- Live pauses among the given owners and daemon_ids. The result is a superset
-- of the owner/daemon_id pairs asked for; callers match the exact pair. A pause
-- past its auto-expiry no longer holds the machine.
SELECT * FROM daemon_pause
WHERE machine_owner_id = ANY(@machine_owner_ids::uuid[])
  AND daemon_id = ANY(@daemon_ids::text[])
  AND paused_at + make_interval(secs => CASE reason WHEN 'maintenance' THEN @maintenance_secs::int ELSE @other_secs::int END) > now();

-- name: CountRunningTasksByMachine :many
-- Tasks still occupying a machine, in any workspace it serves: handed to the
-- daemon or running. Queued tasks do not count; they wait on the server while
-- the daemon is paused. A superset of the pairs asked for, like the pause list.
SELECT COALESCE(rt.owner_id, rt.workspace_id)::uuid AS machine_owner_id, rt.daemon_id::text AS daemon_id, COUNT(*)::int AS running_tasks
FROM agent_task_queue t
JOIN agent_runtime rt ON rt.id = t.runtime_id
WHERE COALESCE(rt.owner_id, rt.workspace_id) = ANY(@machine_owner_ids::uuid[])
  AND rt.daemon_id = ANY(@daemon_ids::text[])
  AND t.status IN ('dispatched', 'running', 'waiting_local_directory')
GROUP BY COALESCE(rt.owner_id, rt.workspace_id), rt.daemon_id;

-- name: ListMachineWorkspaceIDs :many
-- Every workspace a machine serves: the ones that must hear about a pause,
-- resume or drain change.
SELECT DISTINCT workspace_id FROM agent_runtime
WHERE COALESCE(owner_id, workspace_id) = $1 AND daemon_id = $2;

-- name: ListMachinesOfWorkspace :many
-- Machines a workspace's runtimes sit on, plus machines held by a pause this
-- workspace set. Deleting the workspace removes both, which can lift a pause
-- for the owner's other workspaces; they hear about it whether or not a pause
-- existed when this was read.
SELECT COALESCE(rt.owner_id, rt.workspace_id)::uuid AS machine_owner_id, rt.daemon_id::text AS daemon_id
FROM agent_runtime rt
WHERE rt.workspace_id = $1 AND rt.daemon_id IS NOT NULL
UNION
SELECT dp.machine_owner_id, dp.daemon_id FROM daemon_pause dp WHERE dp.workspace_id = $1;
