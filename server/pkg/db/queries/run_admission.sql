-- Dispatch-time admission (CHE-5213). A run keeps status 'queued' while held or
-- reserved; admission_state carries the derived state.

-- name: AcquireProjectAdmissionLock :exec
-- The one lock every start path takes before it counts WIP, so admissions of
-- one project serialize. A transaction advisory lock, not FOR UPDATE on the
-- project row: every task write already holds FOR SHARE on that row
-- (lock_active_project), so two claims upgrading to FOR UPDATE would deadlock.
SELECT pg_advisory_xact_lock(hashtextextended('run_admission:' || @project_id::text, 0));

-- name: GetProjectAdmission :one
SELECT id, workspace_id, status, settings FROM project WHERE id = $1;

-- name: CountProjectWIP :one
-- Runs of the project's issues that hold a slot: dispatched, running or
-- reserved. excluded_task_id lets a claim leave out its own row.
SELECT count(*)::bigint FROM agent_task_queue t
LEFT JOIN issue i ON i.id = t.issue_id
LEFT JOIN autopilot_run ar ON ar.id = t.autopilot_run_id AND t.issue_id IS NULL
LEFT JOIN autopilot ap ON ap.id = ar.autopilot_id
WHERE COALESCE(i.project_id,
         CASE WHEN t.context ->> 'type' = 'quick_create' THEN NULLIF(t.context ->> 'project_id', '')::uuid END,
         ap.project_id) = @project_id
  AND (sqlc.narg('excluded_task_id')::uuid IS NULL OR t.id <> sqlc.narg('excluded_task_id')::uuid)
  AND (t.status IN ('dispatched', 'running', 'waiting_local_directory') OR t.admission_state = 'reserved');

-- name: HoldClaimedTask :one
-- Sends a run claimed in this transaction back to the queue as held.
UPDATE agent_task_queue
SET status = 'queued', dispatched_at = NULL, prepare_lease_expires_at = NULL,
    admission_state = 'held', hold_reason = @hold_reason,
    reservation_lease_id = NULL, reservation_expires_at = NULL
WHERE id = @id AND status = 'dispatched'
RETURNING *;

-- name: ListHeldProjectTasks :many
-- Held runs of one project in focus order: priority (kept in step with the
-- issue's priority), then issue creation time.
SELECT t.id, t.issue_id, t.agent_id FROM agent_task_queue t
LEFT JOIN issue i ON i.id = t.issue_id
LEFT JOIN autopilot_run ar ON ar.id = t.autopilot_run_id AND t.issue_id IS NULL
LEFT JOIN autopilot ap ON ap.id = ar.autopilot_id
WHERE COALESCE(i.project_id,
         CASE WHEN t.context ->> 'type' = 'quick_create' THEN NULLIF(t.context ->> 'project_id', '')::uuid END,
         ap.project_id) = $1 AND t.status = 'queued' AND t.admission_state = 'held'
ORDER BY t.priority DESC, COALESCE(i.created_at, t.created_at) ASC, t.created_at ASC, t.id ASC
FOR UPDATE OF t SKIP LOCKED;

-- name: ReserveHeldTask :one
UPDATE agent_task_queue
SET admission_state = 'reserved', hold_reason = NULL,
    reservation_lease_id = @lease_id,
    reservation_expires_at = now() + make_interval(secs => @lease_secs::double precision)
WHERE id = @id AND status = 'queued' AND admission_state = 'held'
RETURNING *;

-- name: UnholdTask :one
-- Admits a held run with no slot to reserve (no WIP limit).
UPDATE agent_task_queue
SET admission_state = NULL, hold_reason = NULL
WHERE id = $1 AND status = 'queued' AND admission_state = 'held'
RETURNING *;

-- name: ExpireReservation :one
-- Conditional on the lease, so a claim and the expiry cannot both win. SKIP
-- LOCKED: a claim holds the row first and then waits for the admission lock, so
-- waiting on the row here would deadlock; a locked row means a claim is winning.
UPDATE agent_task_queue
SET admission_state = 'held', hold_reason = 'reservation expired',
    reservation_lease_id = NULL, reservation_expires_at = NULL
WHERE agent_task_queue.id = (SELECT q.id FROM agent_task_queue q WHERE q.id = @id FOR UPDATE SKIP LOCKED)
  AND agent_task_queue.status = 'queued' AND agent_task_queue.admission_state = 'reserved'
  AND agent_task_queue.reservation_lease_id = @lease_id
RETURNING *;

-- name: ListExpiredReservations :many
SELECT t.id, t.reservation_lease_id, COALESCE(i.project_id,
         CASE WHEN t.context ->> 'type' = 'quick_create' THEN NULLIF(t.context ->> 'project_id', '')::uuid END,
         ap.project_id) AS project_id FROM agent_task_queue t
LEFT JOIN issue i ON i.id = t.issue_id
LEFT JOIN autopilot_run ar ON ar.id = t.autopilot_run_id AND t.issue_id IS NULL
LEFT JOIN autopilot ap ON ap.id = ar.autopilot_id
WHERE t.status = 'queued' AND t.admission_state = 'reserved'
  AND t.reservation_expires_at <= now()
ORDER BY t.reservation_expires_at ASC
LIMIT 200;

-- name: ListProjectsWithHeldRuns :many
-- The startup reconcile: projects that still have held or reserved runs.
SELECT DISTINCT COALESCE(i.project_id,
         CASE WHEN t.context ->> 'type' = 'quick_create' THEN NULLIF(t.context ->> 'project_id', '')::uuid END,
         ap.project_id) AS project_id FROM agent_task_queue t
LEFT JOIN issue i ON i.id = t.issue_id
LEFT JOIN autopilot_run ar ON ar.id = t.autopilot_run_id AND t.issue_id IS NULL
LEFT JOIN autopilot ap ON ap.id = ar.autopilot_id
WHERE t.status = 'queued' AND t.admission_state IS NOT NULL
  AND COALESCE(i.project_id,
         CASE WHEN t.context ->> 'type' = 'quick_create' THEN NULLIF(t.context ->> 'project_id', '')::uuid END,
         ap.project_id) IS NOT NULL;

-- name: InsertRunInput :one
-- One row per trigger. A retry of the same event id changes nothing.
INSERT INTO run_input (workspace_id, issue_id, agent_id, event_id, kind, payload)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (issue_id, agent_id, event_id) DO NOTHING
RETURNING *;

-- name: BindHeldRunInputs :many
-- The claim moves every held input of (issue, agent) to the run, in arrival order.
UPDATE run_input SET state = 'claimed', task_id = @task_id, updated_at = clock_timestamp()
WHERE issue_id = @issue_id AND agent_id = @agent_id AND state = 'held'
RETURNING *;

-- name: ListRunInputsForTask :many
SELECT * FROM run_input WHERE task_id = $1 AND state = 'claimed' ORDER BY seq ASC;

-- name: DeliverRunInputs :exec
UPDATE run_input SET state = 'delivered', updated_at = clock_timestamp()
WHERE task_id = $1 AND state = 'claimed';

-- name: ParkRunInputs :exec
UPDATE run_input SET state = 'parked', park_reason = @park_reason, failure_class = @failure_class,
    updated_at = clock_timestamp()
WHERE task_id = @task_id AND state = 'claimed';

-- name: ListParkedRunInputs :many
SELECT * FROM run_input
WHERE issue_id = $1 AND agent_id = $2 AND state = 'parked'
ORDER BY seq ASC;

-- name: ListIssueRunInputs :many
SELECT * FROM run_input WHERE issue_id = $1 AND state IN ('held', 'claimed', 'parked') ORDER BY seq ASC;

-- name: ReheldParkedRunInputs :many
-- Returns parked inputs to held. redelivery_of_task_id marks them as a redelivery
-- of the earlier run.
UPDATE run_input SET state = 'held', redelivery_of_task_id = COALESCE(redelivery_of_task_id, task_id),
    task_id = NULL, updated_at = clock_timestamp()
WHERE issue_id = @issue_id AND agent_id = @agent_id AND state = 'parked'
  AND (sqlc.narg('from_task_id')::uuid IS NULL OR task_id = sqlc.narg('from_task_id')::uuid)
RETURNING *;

-- name: DiscardParkedRunInputs :many
UPDATE run_input SET state = 'discarded', updated_at = clock_timestamp()
WHERE issue_id = $1 AND agent_id = $2 AND state = 'parked'
RETURNING *;

-- name: SetProjectWIPLimit :one
-- NULL removes the limit (unlimited, the default).
UPDATE project
SET settings = CASE WHEN sqlc.narg('wip_limit')::int IS NULL THEN settings - 'wip_limit'
                    ELSE jsonb_set(settings, '{wip_limit}', to_jsonb(sqlc.narg('wip_limit')::int)) END,
    updated_at = now()
WHERE id = @id AND workspace_id = @workspace_id
RETURNING *;

-- name: ParkHeldRunInputs :exec
-- A run that ends before any claim (cancelled while held) parks the inputs it
-- was holding; no new trigger pulls them back.
UPDATE run_input SET state = 'parked', park_reason = @park_reason, failure_class = @failure_class,
    updated_at = clock_timestamp()
WHERE issue_id = @issue_id AND agent_id = @agent_id AND state = 'held';

-- name: HasQueuedRetryChild :one
SELECT EXISTS (
    SELECT 1 FROM agent_task_queue WHERE parent_task_id = $1 AND status = 'queued'
);

-- name: ProjectHasHeldRuns :one
SELECT EXISTS (
    SELECT 1 FROM agent_task_queue t
    LEFT JOIN issue i ON i.id = t.issue_id
    LEFT JOIN autopilot_run ar ON ar.id = t.autopilot_run_id AND t.issue_id IS NULL
    LEFT JOIN autopilot ap ON ap.id = ar.autopilot_id
    WHERE COALESCE(i.project_id,
             CASE WHEN t.context ->> 'type' = 'quick_create' THEN NULLIF(t.context ->> 'project_id', '')::uuid END,
             ap.project_id) = $1 AND t.status = 'queued' AND t.admission_state = 'held'
);

-- name: GetTaskProjectID :one
-- The project a run belongs to: its issue's, a quick-create run's chosen
-- project, or a run-only autopilot's. NULL for chat and project-less runs.
SELECT COALESCE(i.project_id,
         CASE WHEN t.context ->> 'type' = 'quick_create' THEN NULLIF(t.context ->> 'project_id', '')::uuid END,
         ap.project_id) AS project_id FROM agent_task_queue t
LEFT JOIN issue i ON i.id = t.issue_id
LEFT JOIN autopilot_run ar ON ar.id = t.autopilot_run_id AND t.issue_id IS NULL
LEFT JOIN autopilot ap ON ap.id = ar.autopilot_id
WHERE t.id = $1;

-- name: ParkOrphanedRunInputs :many
-- Held inputs of an (issue, agent) with no queued run to carry them: nothing
-- would ever start them, so they are parked for a member to resume or discard.
-- A run and its input commit together, so a live input always has its run.
UPDATE run_input r
SET state = 'parked', park_reason = 'orphaned', updated_at = clock_timestamp()
WHERE r.state = 'held'
  AND NOT EXISTS (
      SELECT 1 FROM agent_task_queue t
      WHERE t.issue_id = r.issue_id AND t.agent_id = r.agent_id AND t.status IN ('queued', 'deferred')
  )
RETURNING r.*;
