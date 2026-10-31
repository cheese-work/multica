-- name: ListProjects :many
SELECT * FROM project
WHERE workspace_id = $1
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status'))
  AND (sqlc.narg('priority')::text IS NULL OR priority = sqlc.narg('priority'))
ORDER BY created_at DESC;

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

-- name: ListProjectWakeups :many
SELECT w.* FROM issue_wakeup w JOIN issue i ON i.id=w.issue_id AND i.workspace_id=w.workspace_id
WHERE i.project_id=$1 AND i.workspace_id=$2
-- The workflow_edges watch is the platform's own, not a rule to settle: it
-- stays and waits out an inactive project in its evaluator.
AND w.system_rule IS DISTINCT FROM 'workflow_edges'
ORDER BY w.id FOR UPDATE OF w;

-- name: BumpProjectWakeupRevisions :exec
UPDATE issue_wakeup w SET revision=w.revision+1
FROM issue i WHERE i.id=w.issue_id AND i.workspace_id=w.workspace_id
AND i.project_id=$1 AND i.workspace_id=$2;

-- name: CancelProjectAgentTasks :many
UPDATE agent_task_queue t
SET status='cancelled', completed_at=now(), prepare_lease_expires_at=NULL,
    cancelled_by_type='system', cancelled_by_id=NULL, cancelled_by_name=NULL
WHERE t.status IN ('queued','dispatched','running','waiting_local_directory','deferred')
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

-- name: LockProjectForDelete :one
-- Serializes project deletion with chat-session creation. The handler locks,
-- clears every soft chat reference, and deletes the project in one transaction.
SELECT id FROM project
WHERE id = $1 AND workspace_id = $2
FOR UPDATE;

-- name: CreateProject :one
INSERT INTO project (
    workspace_id, title, description, icon, status,
    lead_type, lead_id, priority, start_date, due_date, workflow_id
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
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
SELECT project_id,
       count(*)::bigint AS total_count,
       count(*) FILTER (WHERE status = ANY(sqlc.arg('terminal_status_keys')::text[]))::bigint AS done_count
FROM issue
WHERE workspace_id = sqlc.arg('workspace_id')::uuid
  AND project_id = ANY(sqlc.arg('project_ids')::uuid[])
GROUP BY project_id;
