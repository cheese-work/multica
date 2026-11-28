-- name: UpsertDaemonPause :one
-- One row per machine: a second pause, from any workspace the machine serves,
-- replaces the reason, actor and clock, so the gate and expires_at share a row.
INSERT INTO daemon_pause (machine_owner_id, daemon_id, workspace_id, reason, paused_by)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (machine_owner_id, daemon_id) DO UPDATE
SET workspace_id = EXCLUDED.workspace_id, reason = EXCLUDED.reason,
    paused_by = EXCLUDED.paused_by, paused_at = now(), drained_notified_at = NULL,
    cycle_id = NULL, expires_at = NULL
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
  AND COALESCE(expires_at, paused_at + make_interval(secs => CASE reason WHEN 'maintenance' THEN @maintenance_secs::int ELSE @other_secs::int END)) > now();

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
-- The machine can span workspaces: the device label comes only from this
-- workspace's runtimes; online state is machine-wide. online_events counts the
-- machine's committed online transitions and generation is the one-row epoch
-- (CHE-9254): together they make the offline period id.
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
       dp.reason AS pause_reason, dp.paused_at AS paused_at, dp.expires_at AS pause_expires_at,
       m.device_name::text AS device_name,
       COALESCE(ev.n, 0)::bigint AS online_events, g.generation AS generation
FROM waiting w
JOIN LATERAL (
    SELECT COALESCE(MIN(NULLIF(o.device_info, '')) FILTER (WHERE o.workspace_id = @workspace_id::uuid),
                    MIN(o.name) FILTER (WHERE o.workspace_id = @workspace_id::uuid)) AS device_name,
           COALESCE(bool_or(o.status = 'online'), false) AS any_online
    FROM agent_runtime o
    WHERE COALESCE(o.owner_id, o.workspace_id) = w.machine_owner_id AND o.daemon_id = w.daemon_id
) m ON true
LEFT JOIN LATERAL (
    SELECT count(*)::bigint AS n
    FROM machine_online_event e
    WHERE e.machine_owner_id = w.machine_owner_id AND e.daemon_id = w.daemon_id
) ev ON true
LEFT JOIN machine_online_event_generation g ON true
LEFT JOIN daemon_pause dp
  ON dp.machine_owner_id = w.machine_owner_id AND dp.daemon_id = w.daemon_id
 AND COALESCE(dp.expires_at, dp.paused_at + make_interval(secs => CASE dp.reason WHEN 'maintenance' THEN @maintenance_secs::int ELSE @other_secs::int END)) > now()
WHERE dp.machine_owner_id IS NOT NULL OR NOT m.any_online
ORDER BY w.machine_owner_id, w.daemon_id;

-- name: GetLiveDaemonPauseByRuntime :one
-- The heartbeat's lookup: the live pause of the machine a runtime sits on, by
-- runtime id, so the WS heartbeat needs no separate runtime read.
SELECT dp.* FROM daemon_pause dp
JOIN agent_runtime rt ON COALESCE(rt.owner_id, rt.workspace_id) = dp.machine_owner_id AND rt.daemon_id = dp.daemon_id
WHERE rt.id = $1
  AND COALESCE(dp.expires_at, dp.paused_at + make_interval(secs => CASE dp.reason WHEN 'maintenance' THEN @maintenance_secs::int ELSE @other_secs::int END)) > now();

-- name: ClaimDaemonPauseDrainNotice :one
-- Marks this pause (identified by its paused_at) as notified. No row means it
-- was already notified or the machine was paused again since the caller read it.
UPDATE daemon_pause SET drained_notified_at = now()
WHERE machine_owner_id = $1 AND daemon_id = $2 AND paused_at = $3 AND drained_notified_at IS NULL
RETURNING *;

-- name: ReleaseDaemonPauseDrainNotice :exec
-- Undoes the claim when the notice could not be written, so the next heartbeat retries.
UPDATE daemon_pause SET drained_notified_at = NULL
WHERE machine_owner_id = $1 AND daemon_id = $2 AND paused_at = $3;

-- name: AcquireCyclePause :one
-- A scheduled update cycle takes the machine's pause (CHE-5466, D10). One
-- statement: insert, or take the row over only when the existing pause has
-- expired. No row returned means a live pause exists and nothing changed.
INSERT INTO daemon_pause (machine_owner_id, daemon_id, workspace_id, reason, paused_by, cycle_id, expires_at)
VALUES (@machine_owner_id, @daemon_id, @workspace_id, 'scheduled_update', NULL, @cycle_id, @expires_at)
ON CONFLICT (machine_owner_id, daemon_id) DO UPDATE
SET workspace_id = EXCLUDED.workspace_id, reason = EXCLUDED.reason, paused_by = NULL,
    paused_at = now(), drained_notified_at = NULL,
    cycle_id = EXCLUDED.cycle_id, expires_at = EXCLUDED.expires_at
WHERE COALESCE(daemon_pause.expires_at, daemon_pause.paused_at + make_interval(secs => CASE daemon_pause.reason WHEN 'maintenance' THEN @maintenance_secs::int ELSE @other_secs::int END)) <= now()
RETURNING *;

-- name: ReleaseCyclePause :execrows
-- Releases the pause only while it is still this cycle's. 0 rows: a person's
-- pause replaced it or it is gone; nothing changes.
DELETE FROM daemon_pause
WHERE machine_owner_id = $1 AND daemon_id = $2 AND cycle_id = $3;

-- name: GetOpenDaemonUpdateCycle :one
-- The machine's open cycle, locked: the lock order is machine lock, pause row, cycle row.
SELECT * FROM daemon_update_cycle
WHERE machine_owner_id = $1 AND daemon_id = $2 AND finished_at IS NULL
FOR UPDATE;

-- name: InsertDaemonUpdateCycle :one
INSERT INTO daemon_update_cycle (id, machine_owner_id, daemon_id, fire_at, started_at, deadline_at, state)
VALUES (@id, @machine_owner_id, @daemon_id, @fire_at, @started_at, @deadline_at, @state)
RETURNING *;

-- name: InsertSkippedDaemonUpdateCycle :one
-- A cron time that cannot open a cycle: written already finished, so it never
-- takes the open-cycle slot.
INSERT INTO daemon_update_cycle (id, machine_owner_id, daemon_id, fire_at, started_at, deadline_at, state, outcome, outcome_source, finished_at)
VALUES (@id, @machine_owner_id, @daemon_id, @fire_at, @started_at, @deadline_at, @outcome, @outcome, 'server', now())
RETURNING *;

-- name: SetDaemonUpdateCycleDraining :one
UPDATE daemon_update_cycle SET state = 'draining' WHERE id = $1 RETURNING *;

-- name: FinishDaemonUpdateCycleSkipped :one
UPDATE daemon_update_cycle
SET state = @outcome, outcome = @outcome, outcome_source = 'server', finished_at = now()
WHERE id = @id
RETURNING *;

-- name: RequestOpenCycleCancel :exec
-- A person's pause or resume cancels the machine's open cycle. The first
-- request stays.
UPDATE daemon_update_cycle SET cancel_requested = COALESCE(cancel_requested, $3)
WHERE machine_owner_id = $1 AND daemon_id = $2 AND finished_at IS NULL;

-- name: GetDaemonUpdateCycle :one
SELECT * FROM daemon_update_cycle WHERE id = $1;

-- name: ListMachineRuntimes :many
-- Every runtime on a machine, online ones first, then the most recently seen.
SELECT * FROM agent_runtime
WHERE COALESCE(owner_id, workspace_id) = $1 AND daemon_id = $2
ORDER BY (status = 'online') DESC, last_seen_at DESC NULLS LAST, id;

-- name: GetDaemonUpdateCycleByFire :one
-- The cycle a cron time already produced; the fire step is idempotent on it.
SELECT * FROM daemon_update_cycle
WHERE machine_owner_id = $1 AND daemon_id = $2 AND fire_at = $3;

-- name: ListDrainingDaemonUpdateCycles :many
SELECT * FROM daemon_update_cycle
WHERE state = 'draining' AND finished_at IS NULL
ORDER BY started_at;

-- name: LockDaemonUpdateCycle :one
SELECT * FROM daemon_update_cycle WHERE id = $1 FOR UPDATE;

-- name: FinishDaemonUpdateCycle :one
-- Conditional on (id, state): a second driver finds no row and does nothing.
UPDATE daemon_update_cycle
SET state = @outcome, outcome = @outcome, outcome_detail = sqlc.narg('outcome_detail'),
    outcome_source = 'server', live_runs_at_end = sqlc.narg('live_runs_at_end'), finished_at = now()
WHERE id = @id AND state = @from_state AND finished_at IS NULL
RETURNING *;

-- name: CycleHoldsLivePause :one
-- Is the machine's pause still this cycle's, and not yet expired at @now?
SELECT EXISTS (
    SELECT 1 FROM daemon_pause
    WHERE machine_owner_id = @machine_owner_id AND daemon_id = @daemon_id
      AND cycle_id = @cycle_id AND expires_at > @now::timestamptz
)::bool AS holds;

-- name: ListLastDaemonUpdateOutcomes :many
-- The latest finished cycle of each machine asked for (a superset of the pairs;
-- callers match the exact pair).
SELECT DISTINCT ON (machine_owner_id, daemon_id) machine_owner_id, daemon_id, outcome, finished_at
FROM daemon_update_cycle
WHERE machine_owner_id = ANY(@machine_owner_ids::uuid[])
  AND daemon_id = ANY(@daemon_ids::text[])
  AND finished_at IS NOT NULL
ORDER BY machine_owner_id, daemon_id, finished_at DESC;

-- name: RecordServerDeploy :execrows
-- One row when the running commit differs from the newest row; a restart on
-- the same commit writes none (CHE-5709, D14).
-- ponytail: two replicas starting in the same instant on a new commit can both write; dedupe on read if it matters.
INSERT INTO server_deploy ("commit", version)
SELECT @commit::text, @version::text
WHERE (SELECT d."commit" FROM server_deploy d ORDER BY d.started_at DESC, d.id DESC LIMIT 1) IS DISTINCT FROM @commit::text;

-- name: ListServerDeploysSince :many
SELECT "commit", version, started_at FROM server_deploy
WHERE started_at >= @since
ORDER BY started_at DESC, id DESC;

-- name: ListMachineUpdateCycles :many
-- A machine's cycles, newest first. Keyset page on (started_at DESC, id DESC);
-- the cursor row is excluded.
SELECT * FROM daemon_update_cycle
WHERE machine_owner_id = @machine_owner_id AND daemon_id = @daemon_id
  AND (sqlc.narg('cursor_at')::timestamptz IS NULL
       OR (started_at, id) < (sqlc.narg('cursor_at')::timestamptz, sqlc.narg('cursor_id')::uuid))
ORDER BY started_at DESC, id DESC
LIMIT @row_limit;
