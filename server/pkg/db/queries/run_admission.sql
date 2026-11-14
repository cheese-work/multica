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
LEFT JOIN chat_session cs ON cs.id = t.chat_session_id AND t.issue_id IS NULL
WHERE COALESCE(i.project_id,
         CASE WHEN t.context ->> 'type' = 'quick_create' THEN NULLIF(t.context ->> 'project_id', '')::uuid END,
         ap.project_id, cs.project_id) = @project_id
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
LEFT JOIN chat_session cs ON cs.id = t.chat_session_id AND t.issue_id IS NULL
WHERE COALESCE(i.project_id,
         CASE WHEN t.context ->> 'type' = 'quick_create' THEN NULLIF(t.context ->> 'project_id', '')::uuid END,
         ap.project_id, cs.project_id) = $1 AND t.status = 'queued' AND t.admission_state = 'held'
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
         ap.project_id, cs.project_id) AS project_id FROM agent_task_queue t
LEFT JOIN issue i ON i.id = t.issue_id
LEFT JOIN autopilot_run ar ON ar.id = t.autopilot_run_id AND t.issue_id IS NULL
LEFT JOIN autopilot ap ON ap.id = ar.autopilot_id
LEFT JOIN chat_session cs ON cs.id = t.chat_session_id AND t.issue_id IS NULL
WHERE t.status = 'queued' AND t.admission_state = 'reserved'
  AND t.reservation_expires_at <= now()
ORDER BY t.reservation_expires_at ASC
LIMIT 200;

-- name: ListProjectsWithHeldRuns :many
-- The startup reconcile: projects that still have held or reserved runs.
SELECT DISTINCT COALESCE(i.project_id,
         CASE WHEN t.context ->> 'type' = 'quick_create' THEN NULLIF(t.context ->> 'project_id', '')::uuid END,
         ap.project_id, cs.project_id) AS project_id FROM agent_task_queue t
LEFT JOIN issue i ON i.id = t.issue_id
LEFT JOIN autopilot_run ar ON ar.id = t.autopilot_run_id AND t.issue_id IS NULL
LEFT JOIN autopilot ap ON ap.id = ar.autopilot_id
LEFT JOIN chat_session cs ON cs.id = t.chat_session_id AND t.issue_id IS NULL
WHERE t.status = 'queued' AND t.admission_state IS NOT NULL
  AND COALESCE(i.project_id,
         CASE WHEN t.context ->> 'type' = 'quick_create' THEN NULLIF(t.context ->> 'project_id', '')::uuid END,
         ap.project_id, cs.project_id) IS NOT NULL;

-- name: InsertRunInput :one
-- One row per trigger, owned by the run it was written for. A retry of the same
-- event id changes nothing.
INSERT INTO run_input (workspace_id, issue_id, agent_id, event_id, kind, payload, state, owner_task_id, thread_key)
VALUES (@workspace_id, @issue_id, @agent_id, @event_id, @kind, @payload, 'owned', @owner_task_id, @thread_key)
ON CONFLICT (issue_id, agent_id, event_id) DO NOTHING
RETURNING *;

-- name: InsertCoalescedRunInput :one
-- The trigger a pending run absorbed instead of a new run (CHE-5266 addendum A5).
-- carrier_task_id is the run the coalescing decision read. The statement locks that
-- row and compares, under the lock, its thread with the trigger's own slot thread
-- (the comment's thread root, the zero UUID for a trigger without a comment), for
-- every status. Entitled and queued: owned by the carrier. Entitled and dispatched
-- or running: absorbed, nothing is stored (slice 1 boundary). Everything else (no
-- carrier, another thread, any other status): parked as ambiguous_owner, no
-- owner, thread unknown, so a member can resume it. A retry of the same event id
-- changes nothing.
WITH slot AS (
    SELECT COALESCE(CASE WHEN sqlc.narg('comment_id')::uuid IS NULL THEN NULL
                         ELSE comment_thread_root_id(sqlc.narg('comment_id')::uuid) END,
                    '00000000-0000-0000-0000-000000000000'::uuid) AS thread
), carrier AS (
    SELECT t.id, t.status,
           COALESCE(t.comment_thread_id, '00000000-0000-0000-0000-000000000000'::uuid) AS thread
    FROM agent_task_queue t
    WHERE t.id = sqlc.narg('carrier_task_id')::uuid AND t.issue_id = @issue_id AND t.agent_id = @agent_id
    FOR UPDATE
), verdict AS (
    SELECT s.thread AS slot_thread, c.id AS carrier_id, c.status AS carrier_status,
           COALESCE(c.thread = s.thread, false) AS entitled
    FROM slot s LEFT JOIN carrier c ON true
)
INSERT INTO run_input (workspace_id, issue_id, agent_id, event_id, kind, payload, state, owner_task_id, thread_key, park_reason)
SELECT @workspace_id, @issue_id, @agent_id, @event_id, @kind, @payload,
       CASE WHEN v.entitled AND v.carrier_status = 'queued' THEN 'owned' ELSE 'parked' END,
       CASE WHEN v.entitled AND v.carrier_status = 'queued' THEN v.carrier_id END,
       CASE WHEN v.entitled AND v.carrier_status = 'queued' THEN v.slot_thread END,
       CASE WHEN NOT (v.entitled AND v.carrier_status = 'queued') THEN 'ambiguous_owner' END
FROM verdict v
WHERE NOT (v.entitled AND v.carrier_status IN ('dispatched', 'running'))
ON CONFLICT (issue_id, agent_id, event_id) DO NOTHING
RETURNING *;

-- name: BindOwnedRunInputs :many
-- The claim moves the inputs its run owns to the run, in arrival order. Same
-- issue, same agent or same thread is never proof of ownership.
UPDATE run_input SET state = 'claimed', task_id = @task_id, updated_at = clock_timestamp()
WHERE owner_task_id = @task_id AND state = 'owned'
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
SELECT * FROM run_input WHERE issue_id = $1 AND state IN ('owned', 'claimed', 'parked') ORDER BY seq ASC;

-- name: LockParkedRunInputsForRetry :many
-- The parked inputs a failed parent claimed that belong to the retry child's
-- thread. thread_key is compared with =, so an unknown thread never matches; a
-- quarantined legacy row is never re-held to a retry.
SELECT seq FROM run_input
WHERE task_id = @parent_id AND state = 'parked' AND thread_key = @retry_thread_key
  AND park_reason IS DISTINCT FROM 'legacy_claim_unproven'
  AND park_reason IS DISTINCT FROM 'legacy_unowned'
ORDER BY seq
FOR UPDATE;

-- name: ReheldRunInputsByID :many
-- Returns exact parked rows to owned by one task. The thread guard keeps a row
-- off a carrier of another thread even if a caller passes a wrong id.
UPDATE run_input SET state = 'owned', owner_task_id = @owner_task_id, park_reason = NULL,
    redelivery_of_task_id = COALESCE(redelivery_of_task_id, task_id),
    task_id = NULL, updated_at = clock_timestamp()
WHERE seq = ANY(@ids::bigint[]) AND state = 'parked' AND thread_key = @thread_key
RETURNING seq;

-- name: AssignParkedInputThread :many
-- A member's explicit choice of thread for unknown-thread rows.
UPDATE run_input SET thread_key = @thread_key, updated_at = clock_timestamp()
WHERE seq = ANY(@ids::bigint[]) AND issue_id = @issue_id AND agent_id = @agent_id
  AND state = 'parked' AND thread_key IS NULL
RETURNING seq;

-- name: LockParkedNullThreadRunInputs :many
SELECT seq FROM run_input
WHERE seq = ANY(@ids::bigint[]) AND issue_id = @issue_id AND agent_id = @agent_id
  AND state = 'parked' AND thread_key IS NULL
ORDER BY seq
FOR UPDATE;

-- name: ListParkedRunInputRefs :many
SELECT seq, thread_key FROM run_input
WHERE issue_id = @issue_id AND agent_id = @agent_id AND state = 'parked'
ORDER BY seq;

-- name: LockParkedRunInputsInThread :many
SELECT seq FROM run_input
WHERE seq = ANY(@ids::bigint[]) AND state = 'parked' AND thread_key = @thread_key
ORDER BY seq
FOR UPDATE;

-- name: LockSlotCarrier :one
-- The pending (queued or dispatched) run of one slot, locked. No row: the slot is free.
SELECT * FROM agent_task_queue
WHERE issue_id = @issue_id AND agent_id = @agent_id AND status IN ('queued', 'dispatched')
  AND COALESCE(comment_thread_id, '00000000-0000-0000-0000-000000000000'::uuid) = @thread_key
FOR UPDATE;

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

-- name: ParkOwnedRunInputs :exec
-- A run that ends before any claim parks the inputs it owns, and only those;
-- inputs owned by another run stay owned.
UPDATE run_input SET state = 'parked', park_reason = @park_reason, failure_class = @failure_class,
    owner_task_id = NULL, updated_at = clock_timestamp()
WHERE owner_task_id = @task_id AND state = 'owned';

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
    LEFT JOIN chat_session cs ON cs.id = t.chat_session_id AND t.issue_id IS NULL
    WHERE COALESCE(i.project_id,
             CASE WHEN t.context ->> 'type' = 'quick_create' THEN NULLIF(t.context ->> 'project_id', '')::uuid END,
             ap.project_id, cs.project_id) = $1 AND t.status = 'queued' AND t.admission_state = 'held'
);

-- name: GetTaskProjectID :one
-- The project a run belongs to: its issue's, a quick-create run's chosen
-- project, a run-only autopilot's, or a chat session's. NULL for project-less runs.
SELECT COALESCE(i.project_id,
         CASE WHEN t.context ->> 'type' = 'quick_create' THEN NULLIF(t.context ->> 'project_id', '')::uuid END,
         ap.project_id, cs.project_id) AS project_id FROM agent_task_queue t
LEFT JOIN issue i ON i.id = t.issue_id
LEFT JOIN autopilot_run ar ON ar.id = t.autopilot_run_id AND t.issue_id IS NULL
LEFT JOIN autopilot ap ON ap.id = ar.autopilot_id
LEFT JOIN chat_session cs ON cs.id = t.chat_session_id AND t.issue_id IS NULL
WHERE t.id = $1;

-- name: ParkOrphanedRunInputs :many
-- Owned inputs whose owner is no longer an active run: nothing would ever
-- start them, so they are parked for a member to resume or discard. A held row
-- (none should exist, the retire trigger parks them) is parked as legacy_unowned.
UPDATE run_input r
SET state = 'parked', owner_task_id = NULL,
    park_reason = CASE WHEN r.state = 'held' THEN 'legacy_unowned' ELSE 'orphaned' END,
    updated_at = clock_timestamp()
WHERE (r.state = 'owned'
       AND NOT EXISTS (
           SELECT 1 FROM agent_task_queue t
           WHERE t.id = r.owner_task_id AND t.status IN ('queued', 'dispatched', 'running', 'deferred')
       ))
   OR r.state = 'held'
RETURNING r.*;

-- name: InsertAdmissionRelease :exec
-- The release an ended run owes its project (CHE-5265). Written in the run's
-- terminal transaction; a repeat for the same run changes nothing.
INSERT INTO admission_release (task_id, project_id) VALUES (@task_id, @project_id)
ON CONFLICT (task_id) DO NOTHING;

-- name: ListAdmissionReleaseTasks :many
SELECT task_id FROM admission_release WHERE project_id = $1;

-- name: DeleteAdmissionReleases :exec
-- Deletes exactly the rows a release read; rows inserted since stay.
DELETE FROM admission_release WHERE task_id = ANY(@task_ids::uuid[]);

-- name: ListAdmissionReleaseProjects :many
-- The job's work list: projects that owe a release. The table stays small
-- because a release deletes what it delivered.
SELECT DISTINCT project_id FROM admission_release;

-- name: CountHeldProjectTasks :one
-- Held runs of one project, without a lock: ListHeldProjectTasks skips locked
-- rows, so count minus listed is the number it skipped.
SELECT count(*)::bigint FROM agent_task_queue t
LEFT JOIN issue i ON i.id = t.issue_id
LEFT JOIN autopilot_run ar ON ar.id = t.autopilot_run_id AND t.issue_id IS NULL
LEFT JOIN autopilot ap ON ap.id = ar.autopilot_id
LEFT JOIN chat_session cs ON cs.id = t.chat_session_id AND t.issue_id IS NULL
WHERE COALESCE(i.project_id,
         CASE WHEN t.context ->> 'type' = 'quick_create' THEN NULLIF(t.context ->> 'project_id', '')::uuid END,
         ap.project_id, cs.project_id) = $1 AND t.status = 'queued' AND t.admission_state = 'held';

-- name: LockOwnedRunInputs :many
-- The inputs a run still owns, locked. The task row is locked first.
SELECT * FROM run_input WHERE owner_task_id = @owner_task_id AND state = 'owned' ORDER BY seq FOR UPDATE;

-- name: MoveOwnedRunInputs :exec
-- Hands the inputs of one owner to another. The thread guard keeps a row off a
-- carrier of another thread; unmatched rows stay with the old owner.
UPDATE run_input SET owner_task_id = @new_owner_task_id, updated_at = clock_timestamp()
WHERE owner_task_id = @old_owner_task_id AND state = 'owned' AND thread_key = @thread_key;

-- name: GetCommentInIssue :one
-- A thread key names a comment of this issue.
SELECT id FROM comment WHERE id = @id AND issue_id = @issue_id;
