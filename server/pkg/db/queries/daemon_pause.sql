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

-- name: LockWorkspaceForMachinePause :one
-- Pause and Resume share this row with the workspace delete's FOR UPDATE, so a
-- pause is either in the delete's machine read or refused once the row is gone.
SELECT id FROM workspace WHERE id = $1 FOR SHARE;

-- name: ListAffectedMachines :many
-- Machines that hold queued tasks of this workspace and are off: paused, or with
-- no online runtime. all_projects is the workspace owner's view; otherwise only
-- tasks of projects led by user_id count. A machine with only running tasks, or
-- none, is not affected: nothing waits for it.
WITH waiting AS (
    SELECT COALESCE(rt.owner_id, rt.workspace_id)::uuid AS machine_owner_id, rt.daemon_id::text AS daemon_id, COUNT(*)::int AS waiting_tasks
    FROM agent_task_queue t
    JOIN agent_runtime rt ON rt.id = t.runtime_id
    LEFT JOIN issue i ON i.id = t.issue_id
    LEFT JOIN project p ON p.id = i.project_id
    WHERE t.status = 'queued'
      AND rt.workspace_id = @workspace_id::uuid
      AND rt.daemon_id IS NOT NULL
      AND (@all_projects::bool OR (p.lead_type = 'member' AND p.lead_id = @user_id::uuid))
    GROUP BY 1, 2
)
SELECT w.machine_owner_id, w.daemon_id, w.waiting_tasks,
       dp.reason AS pause_reason, dp.paused_at AS paused_at,
       m.device_name::text AS device_name, m.last_seen_at::timestamptz AS last_seen_at
FROM waiting w
JOIN LATERAL (
    SELECT COALESCE(MIN(NULLIF(o.device_info, '')), MIN(o.name)) AS device_name,
           MAX(o.last_seen_at) AS last_seen_at,
           COALESCE(bool_or(o.status = 'online'), false) AS any_online
    FROM agent_runtime o
    WHERE COALESCE(o.owner_id, o.workspace_id) = w.machine_owner_id AND o.daemon_id = w.daemon_id
) m ON true
LEFT JOIN daemon_pause dp
  ON dp.machine_owner_id = w.machine_owner_id AND dp.daemon_id = w.daemon_id
 AND dp.paused_at + make_interval(secs => CASE dp.reason WHEN 'maintenance' THEN @maintenance_secs::int ELSE @other_secs::int END) > now()
WHERE dp.machine_owner_id IS NOT NULL OR NOT m.any_online
ORDER BY w.machine_owner_id, w.daemon_id;
