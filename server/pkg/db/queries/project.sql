-- name: ListProjects :many
SELECT * FROM project
WHERE workspace_id = $1
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status'))
  AND (sqlc.narg('priority')::text IS NULL OR priority = sqlc.narg('priority'))
  AND (sqlc.arg('include_archived')::boolean OR archived_at IS NULL)
ORDER BY created_at DESC;

-- name: ArchiveProject :one
-- Guarded on archived_at IS NULL so a repeated archive keeps the first
-- actor and time; no row means it was already archived (or is gone).
UPDATE project SET
    archived_at = now(),
    archived_by_type = sqlc.narg('archived_by_type'),
    archived_by_id = sqlc.narg('archived_by_id'),
    updated_at = now()
WHERE id = $1 AND workspace_id = $2 AND archived_at IS NULL
RETURNING *;

-- name: RestoreProject :one
UPDATE project SET
    archived_at = NULL,
    archived_by_type = NULL,
    archived_by_id = NULL,
    updated_at = now()
WHERE id = $1 AND workspace_id = $2 AND archived_at IS NOT NULL
RETURNING *;

-- name: GetProjectInWorkspace :one
SELECT * FROM project
WHERE id = $1 AND workspace_id = $2;

-- name: RequireActiveProject :one
WITH workspace_lock AS MATERIALIZED (
    SELECT id FROM workspace WHERE id=sqlc.arg(workspace_id)::uuid FOR KEY SHARE
)
SELECT p.status FROM project p JOIN workspace_lock w ON w.id=p.workspace_id
WHERE p.id=sqlc.narg(project_id)::uuid FOR SHARE OF p;

-- name: GetIssueProjectStatus :one
SELECT p.status FROM project p JOIN issue i ON i.project_id=p.id AND i.workspace_id=p.workspace_id
WHERE i.id=$1;

-- name: LockIssueProjectStatus :one
SELECT p.status FROM project p JOIN issue i ON i.project_id=p.id AND i.workspace_id=p.workspace_id
WHERE i.id=$1 FOR SHARE OF p;

-- A status flip settles a project's wakeups with set-based statements, one per
-- step, so its cost does not grow with the number of rules (CHE-9000: a
-- per-rule loop in the flip's transaction timed out resuming a 5,365-issue
-- project). The rules in scope are the enabled ones that are not the
-- platform's own workflow_edges watch (it waits out an inactive project in its
-- evaluator) or pr_merge_owed. A disabled rule cannot fire, so it has nothing
-- to settle (CHE-7652); the revision bump still covers it.

-- name: DiscardProjectWakeupReceipts :exec
UPDATE issue_wakeup_receipt r SET processed_at=now()
FROM issue_wakeup w JOIN issue i ON i.id=w.issue_id AND i.workspace_id=w.workspace_id
WHERE r.wakeup_id=w.id AND r.processed_at IS NULL
AND i.project_id=$1 AND i.workspace_id=$2 AND w.enabled
AND w.system_rule IS DISTINCT FROM 'workflow_edges'
AND w.system_rule IS DISTINCT FROM 'pr_merge_owed';

-- name: MarkProjectWakeupsSettled :exec
-- Sets the settle error (project_inactive on a pause, null on a resume). A
-- condition rule is held until hold_seconds from now: the baseline pass after
-- the commit re-polls it sooner, and if that pass never runs the rule falls
-- back to its ordinary poll once the hold ends.
UPDATE issue_wakeup w SET last_error=sqlc.narg(last_error), updated_at=clock_timestamp(),
 next_fire_at=CASE WHEN w.condition IS NOT NULL THEN now()+make_interval(secs => sqlc.arg(hold_seconds)::int) ELSE w.next_fire_at END
FROM issue i
WHERE i.id=w.issue_id AND i.workspace_id=w.workspace_id
AND i.project_id=sqlc.arg(project_id) AND i.workspace_id=sqlc.arg(workspace_id) AND w.enabled
AND w.system_rule IS DISTINCT FROM 'workflow_edges'
AND w.system_rule IS DISTINCT FROM 'pr_merge_owed';

-- name: DisableProjectDueOneShots :exec
-- A one-shot that came due while the project was inactive does not fire later.
UPDATE issue_wakeup w SET enabled=false, next_fire_at=NULL, updated_at=clock_timestamp()
FROM issue i
WHERE i.id=w.issue_id AND i.workspace_id=w.workspace_id
AND i.project_id=$1 AND i.workspace_id=$2 AND w.enabled
AND w.system_rule IS DISTINCT FROM 'workflow_edges'
AND w.system_rule IS DISTINCT FROM 'pr_merge_owed'
AND w.condition IS NULL AND w.kind='at' AND w.next_fire_at<=now();

-- name: ListProjectOverdueRecurringWakeups :many
SELECT w.* FROM issue_wakeup w JOIN issue i ON i.id=w.issue_id AND i.workspace_id=w.workspace_id
WHERE i.project_id=$1 AND i.workspace_id=$2 AND w.enabled
AND w.system_rule IS DISTINCT FROM 'workflow_edges'
AND w.system_rule IS DISTINCT FROM 'pr_merge_owed'
AND w.condition IS NULL AND w.kind IN ('every','cron') AND w.next_fire_at<=now()
ORDER BY w.id FOR UPDATE OF w;

-- name: AdvanceWakeupsBatch :exec
-- AdvanceIssueWakeup for many rules in one statement; it leaves last_error alone.
UPDATE issue_wakeup w SET enabled=u.enabled AND w.paused_reason IS NULL AND w.disabled_at IS NULL,
 next_fire_at=u.next_fire_at, updated_at=clock_timestamp()
FROM (SELECT unnest(@ids::uuid[]) AS id, unnest(@enabled::bool[]) AS enabled, unnest(@next_fire_at::timestamptz[]) AS next_fire_at) u
WHERE w.id=u.id;

-- name: BumpProjectWakeupRevisions :many
-- Returns the condition rules the baseline pass owns, with the revision and
-- hold the flip left on them. The pass writes a rule only while both still
-- match, so a later flip, edit or poll wins over an older pass.
WITH bumped AS (
 UPDATE issue_wakeup w SET revision=w.revision+1
 FROM issue i WHERE i.id=w.issue_id AND i.workspace_id=w.workspace_id
 AND i.project_id=$1 AND i.workspace_id=$2
 RETURNING w.id, w.revision, w.next_fire_at, w.enabled, w.condition, w.system_rule
)
SELECT id, revision, next_fire_at FROM bumped
WHERE enabled AND condition IS NOT NULL
AND system_rule IS DISTINCT FROM 'workflow_edges'
AND system_rule IS DISTINCT FROM 'pr_merge_owed'
ORDER BY id;

-- name: CancelProjectAgentTasks :many
-- keep_running: a pause drains, so a running run lives on; cancel and complete stop it.
UPDATE agent_task_queue t
SET status='cancelled', completed_at=now(), prepare_lease_expires_at=NULL,
    cancelled_by_type='system', cancelled_by_id=NULL, cancelled_by_name=NULL
WHERE t.status IN ('queued','dispatched','running','waiting_local_directory','deferred')
AND NOT (sqlc.arg(keep_running)::bool AND t.status='running')
AND (t.issue_id IN (SELECT i.id FROM issue i WHERE i.project_id=sqlc.arg(project_id)::uuid AND i.workspace_id=sqlc.arg(workspace_id)::uuid)
 OR (t.issue_id IS NULL AND t.autopilot_run_id IN (SELECT r.id FROM autopilot_run r JOIN autopilot a ON a.id=r.autopilot_id WHERE a.project_id=sqlc.arg(project_id)::uuid AND a.workspace_id=sqlc.arg(workspace_id)::uuid))
 OR (t.issue_id IS NULL AND t.context->>'type'='quick_create' AND t.context->>'project_id'=sqlc.arg(project_id)::uuid::text AND t.agent_id IN (SELECT a.id FROM agent a WHERE a.workspace_id=sqlc.arg(workspace_id)::uuid)))
RETURNING t.*;

-- name: LockProjectForChatSessionCreate :one
-- Conflicts with project deletion so a chat session cannot commit a soft
-- project reference after the delete transaction has swept existing sessions.
SELECT id FROM project
WHERE id = $1 AND workspace_id = $2
FOR KEY SHARE;

-- name: LockProjectArchiveState :one
-- Share-locks the project row and returns its archive bit. Archiving updates
-- the row, so a save that decides on this read serializes with the archive and
-- its autopilot pause sweep instead of reading a pre-archive snapshot.
SELECT archived_at FROM project
WHERE id = $1 AND workspace_id = $2
FOR SHARE;

-- name: LockProjectForDelete :one
-- Serializes project deletion with chat-session creation. The handler locks,
-- clears every soft chat reference, and deletes the project in one transaction.
SELECT id FROM project
WHERE id = $1 AND workspace_id = $2
FOR UPDATE;

-- name: CreateProject :one
INSERT INTO project (
    workspace_id, title, description, icon, status,
    lead_type, lead_id, priority, start_date, due_date, workflow_id,
    creator_type, creator_id
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13
) RETURNING *;

-- name: UpdateProject :one
UPDATE project SET
    title = COALESCE(sqlc.narg('title'), title),
    description = CASE WHEN sqlc.arg('description_set')::boolean THEN sqlc.narg('description') ELSE description END,
    icon = CASE WHEN sqlc.arg('icon_set')::boolean THEN sqlc.narg('icon') ELSE icon END,
    status = COALESCE(sqlc.narg('status'), status),
    priority = COALESCE(sqlc.narg('priority'), priority),
    lead_type = CASE WHEN sqlc.arg('lead_type_set')::boolean THEN sqlc.narg('lead_type') ELSE lead_type END,
    lead_id = CASE WHEN sqlc.arg('lead_id_set')::boolean THEN sqlc.narg('lead_id') ELSE lead_id END,
    start_date = CASE WHEN sqlc.arg('start_date_set')::boolean THEN sqlc.narg('start_date') ELSE start_date END,
    due_date = CASE WHEN sqlc.arg('due_date_set')::boolean THEN sqlc.narg('due_date') ELSE due_date END,
    decision_delegates = CASE WHEN sqlc.arg('decision_delegates_set')::boolean THEN sqlc.arg('decision_delegates')::uuid[] ELSE decision_delegates END,
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: DeleteProject :exec
-- Defense-in-depth: workspace_id is a SQL-layer tenant guard. See DeleteIssue.
DELETE FROM project WHERE id = $1 AND workspace_id = $2;

-- name: CountIssuesByProject :one
SELECT count(*) FROM issue
WHERE project_id = $1;

-- name: GetProjectIssueStats :many
-- done_count counts every terminal issue (done and closed categories) and
-- keeps that meaning for older clients; completed_count is the done category
-- alone, so cancelled = done_count - completed_count.
SELECT project_id,
       count(*)::bigint AS total_count,
       count(*) FILTER (WHERE status = ANY(sqlc.arg('terminal_status_keys')::text[]))::bigint AS done_count,
       count(*) FILTER (WHERE status = ANY(sqlc.arg('completed_status_keys')::text[]))::bigint AS completed_count,
       count(*) FILTER (WHERE status = ANY(sqlc.arg('started_status_keys')::text[]))::bigint AS started_count,
       max(GREATEST(updated_at, last_activity_at))::timestamptz AS last_issue_activity_at
FROM issue
WHERE workspace_id = sqlc.arg('workspace_id')::uuid
  AND project_id = ANY(sqlc.arg('project_ids')::uuid[])
GROUP BY project_id;
<<<<<<< HEAD

-- name: SetProjectWorkflowSplit :one
-- NULL clears the split (CHE-5262).
UPDATE project SET workflow_split = sqlc.narg('workflow_split')::jsonb, updated_at = now()
WHERE id = $1 AND workspace_id = $2
RETURNING *;
||||||| 2fcb424b72
=======

-- name: ListProjectProgressIssues :many
SELECT id, status, assignee_type, assignee_id, created_at,
       ARRAY(
           SELECT il.label_id FROM issue_to_label il
           WHERE il.issue_id = issue.id
           ORDER BY il.label_id
       )::uuid[] AS label_ids
FROM issue
WHERE workspace_id = sqlc.arg('workspace_id')::uuid
  AND project_id = sqlc.arg('project_id')::uuid;

-- name: ListIssueStatusChangesSince :many
SELECT issue_id, details, created_at
FROM activity_log
WHERE workspace_id = sqlc.arg('workspace_id')::uuid
  AND issue_id = ANY(sqlc.arg('issue_ids')::uuid[])
  AND action = 'status_changed'
  AND created_at >= sqlc.arg('since')::timestamptz
ORDER BY created_at, id;

-- name: ListProjectActivity :many
-- The newest activity on a project's issues, newest first, keyset-paged on
-- (created_at, id). Only the given actions are read, so autosaves and run
-- bookkeeping stay out of the project feed. A zero cursor reads from the top.
SELECT a.id, a.issue_id, a.actor_type, a.actor_id, a.action, a.details, a.created_at,
       i.number AS issue_number, i.title AS issue_title, i.status AS issue_status
FROM activity_log a
JOIN issue i ON i.id = a.issue_id
WHERE i.workspace_id = sqlc.arg('workspace_id')::uuid
  AND i.project_id = sqlc.arg('project_id')::uuid
  AND a.action = ANY(sqlc.arg('actions')::text[])
  AND (
      sqlc.narg('before_at')::timestamptz IS NULL
      OR (a.created_at, a.id) < (sqlc.narg('before_at')::timestamptz, sqlc.narg('before_id')::uuid)
  )
ORDER BY a.created_at DESC, a.id DESC
LIMIT sqlc.arg('row_limit');
>>>>>>> 35b54a3742bb
