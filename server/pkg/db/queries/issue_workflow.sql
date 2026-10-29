-- Project workflows (MUL-7420). A workflow selects and orders statuses from
-- the workspace's issue_status catalog. Projects opt in through
-- project.workflow_id; a project without one uses the workspace's Default
-- workflow row. Each step also stores who an issue entering it is handed to.

-- name: ListIssueWorkflows :many
SELECT * FROM issue_workflow
WHERE workspace_id = sqlc.arg('workspace_id')::uuid
ORDER BY is_default DESC, lower(name), created_at, id;

-- name: GetDefaultIssueWorkflow :one
SELECT * FROM issue_workflow
WHERE workspace_id = sqlc.arg('workspace_id')::uuid
  AND is_default;

-- name: CreateDefaultIssueWorkflow :many
-- Returns no row when another request created the Default first.
INSERT INTO issue_workflow (workspace_id, name, description, initial_status_key, steps, is_default)
VALUES (
    sqlc.arg('workspace_id')::uuid,
    'Default',
    '',
    'todo',
    sqlc.arg('steps')::jsonb,
    true
)
ON CONFLICT (workspace_id) WHERE is_default DO NOTHING
RETURNING *;

-- name: ListNamedIssueWorkflowSteps :many
-- The steps of every workflow except Default, for the Default snapshot.
SELECT steps FROM issue_workflow
WHERE workspace_id = sqlc.arg('workspace_id')::uuid
  AND NOT is_default;

-- name: RemoveStatusFromDefaultIssueWorkflow :exec
-- An archived status leaves Default with it; Default never blocks an archive.
UPDATE issue_workflow SET
    steps = COALESCE((
        SELECT jsonb_agg(step ORDER BY ord)
        FROM jsonb_array_elements(steps) WITH ORDINALITY AS s(step, ord)
        WHERE step->>'status_key' <> sqlc.arg('status_key')::text
    ), '[]'::jsonb),
    updated_at = now()
WHERE workspace_id = sqlc.arg('workspace_id')::uuid
  AND is_default
  AND steps @> jsonb_build_array(jsonb_build_object('status_key', sqlc.arg('status_key')::text));

-- name: ListProjectsUsingDefaultIssueWorkflow :many
SELECT * FROM project
WHERE workspace_id = sqlc.arg('workspace_id')::uuid
  AND workflow_id IS NULL
ORDER BY created_at, id;

-- name: GetIssueWorkflow :one
SELECT * FROM issue_workflow
WHERE id = sqlc.arg('id')::uuid
  AND workspace_id = sqlc.arg('workspace_id')::uuid;

-- name: CreateIssueWorkflow :one
INSERT INTO issue_workflow (workspace_id, name, description, initial_status_key, steps, owner_id, max_runs_per_issue, include_child_prs)
VALUES (
    sqlc.arg('workspace_id')::uuid,
    sqlc.arg('name')::text,
    sqlc.arg('description')::text,
    sqlc.arg('initial_status_key')::text,
    sqlc.arg('steps')::jsonb,
    sqlc.arg('owner_id')::uuid,
    NULLIF(sqlc.narg('max_runs_per_issue')::int, 0),
    COALESCE(sqlc.narg('include_child_prs')::boolean, true)
)
RETURNING *;

-- name: UpdateIssueWorkflow :one
UPDATE issue_workflow SET
    name = sqlc.arg('name')::text,
    description = sqlc.arg('description')::text,
    initial_status_key = sqlc.arg('initial_status_key')::text,
    steps = sqlc.arg('steps')::jsonb,
    -- A rule left out of the write keeps its value; a max of 0 clears it back
    -- to the platform default.
    max_runs_per_issue = CASE
        WHEN sqlc.narg('max_runs_per_issue')::int IS NULL THEN max_runs_per_issue
        ELSE NULLIF(sqlc.narg('max_runs_per_issue')::int, 0)
    END,
    include_child_prs = COALESCE(sqlc.narg('include_child_prs')::boolean, include_child_prs),
    updated_at = now()
WHERE id = sqlc.arg('id')::uuid
  AND workspace_id = sqlc.arg('workspace_id')::uuid
RETURNING *;

-- name: DeleteIssueWorkflow :execrows
DELETE FROM issue_workflow
WHERE id = sqlc.arg('id')::uuid
  AND workspace_id = sqlc.arg('workspace_id')::uuid
  AND NOT is_default;

-- name: DeleteIssueWorkflowsForWorkspace :exec
-- No foreign keys by project rule, so workspace teardown cleans up here.
DELETE FROM issue_workflow WHERE workspace_id = sqlc.arg('workspace_id')::uuid;

-- name: ListIssueWorkflowNamesUsingStatusKey :many
-- Archive guard for the status catalog: a status a workflow still lists cannot
-- be retired, or that workflow's projects would lose a column.
SELECT name FROM issue_workflow
WHERE workspace_id = sqlc.arg('workspace_id')::uuid
  AND NOT is_default
  AND steps @> jsonb_build_array(jsonb_build_object('status_key', sqlc.arg('status_key')::text))
ORDER BY lower(name);

-- name: ListProjectsUsingIssueWorkflow :many
SELECT * FROM project
WHERE workspace_id = sqlc.arg('workspace_id')::uuid
  AND workflow_id = sqlc.arg('workflow_id')::uuid
ORDER BY created_at, id;

-- name: CountProjectIssuesByStatus :many
-- Status census for a workflow switch or step removal, including terminal
-- issues: every issue of the project must end on a status its workflow lists.
-- Issues with their own workflow override follow that workflow, not the
-- project's, so neither the census nor the remap below counts or moves them.
SELECT status, COUNT(*)::bigint AS issue_count
FROM issue
WHERE workspace_id = sqlc.arg('workspace_id')::uuid
  AND project_id = ANY(sqlc.arg('project_ids')::uuid[])
  AND workflow_id IS NULL
GROUP BY status
ORDER BY status;

-- name: RemapProjectIssueStatus :many
-- Moves every issue of the given projects from one status to another as part
-- of an administrative workflow change. Mirrors UpdateIssueStatus: the issue
-- is re-ranked to the top of its new column, the revision advances, and a
-- duplicate mark only survives cancelled -> cancelled (never the case here,
-- because from and to differ). It deliberately touches nothing else, so it
-- never hands an issue off or starts a run.
UPDATE issue AS i SET
    status = sqlc.arg('to_status')::text,
    duplicate_of_issue_id = NULL,
    position = (
        SELECT COALESCE(MIN(target.position), 0) - 1
        FROM issue AS target
        WHERE target.workspace_id = i.workspace_id
          AND target.status = sqlc.arg('to_status')::text
    ),
    revision = i.revision + 1,
    last_activity_at = GREATEST(COALESCE(i.last_activity_at, i.updated_at), now()),
    updated_at = now()
WHERE i.workspace_id = sqlc.arg('workspace_id')::uuid
  AND i.project_id = ANY(sqlc.arg('project_ids')::uuid[])
  AND i.status = sqlc.arg('from_status')::text
  AND i.status <> sqlc.arg('to_status')::text
  AND i.workflow_id IS NULL
RETURNING i.*;

-- name: LockProjectForWorkflowChange :one
-- Serializes concurrent workflow switches of one project.
SELECT * FROM project
WHERE id = sqlc.arg('id')::uuid
  AND workspace_id = sqlc.arg('workspace_id')::uuid
FOR UPDATE;

-- name: LockProjectForIssueWrite :one
-- Held by an issue write that applies the project's workflow. A workflow
-- switch takes the row FOR UPDATE, which conflicts with this lock, so the write
-- either sees the switch or finishes before it starts. KEY SHARE leaves project
-- renames and other row updates unblocked.
SELECT id FROM project
WHERE id = sqlc.arg('id')::uuid
  AND workspace_id = sqlc.arg('workspace_id')::uuid
FOR KEY SHARE;

-- name: SetProjectWorkflow :one
UPDATE project SET
    workflow_id = sqlc.narg('workflow_id')::uuid,
    updated_at = now()
WHERE id = sqlc.arg('id')::uuid
  AND workspace_id = sqlc.arg('workspace_id')::uuid
RETURNING *;

-- name: SetIssueWorkflow :one
-- Sets or clears (NULL) one issue's workflow override. Administrative like a
-- project workflow switch: no status, assignee or run changes.
UPDATE issue SET
    workflow_id = sqlc.narg('workflow_id')::uuid,
    revision = revision + 1,
    updated_at = now()
WHERE id = sqlc.arg('id')::uuid
  AND workspace_id = sqlc.arg('workspace_id')::uuid
RETURNING *;

-- name: HasLiveTaskForIssue :one
-- Any task that can still run or retry. A deferred task is included: its retry
-- keeps the old workflow step, so a switch under it would pass the stale-step
-- check. HasActiveTaskForIssue covers only the first four states.
SELECT EXISTS (
    SELECT 1 FROM agent_task_queue
    WHERE issue_id = sqlc.arg('issue_id')::uuid
      AND status IN ('queued', 'dispatched', 'waiting_local_directory', 'running', 'deferred')
);

-- name: CountIssuesOverridingToIssueWorkflow :one
-- Direct references that block deleting a workflow.
SELECT COUNT(*)::bigint FROM issue
WHERE workspace_id = sqlc.arg('workspace_id')::uuid
  AND workflow_id = sqlc.arg('workflow_id')::uuid;

-- name: ListOverrideIssueStatusesForWorkflow :many
-- Statuses held by issues that follow this workflow by override, for an edit
-- that would drop one of them.
SELECT DISTINCT status FROM issue
WHERE workspace_id = sqlc.arg('workspace_id')::uuid
  AND workflow_id = sqlc.arg('workflow_id')::uuid;

-- name: ClaimIssueCIAdvance :execrows
-- One green head advances an issue once. Zero rows means this head already did.
INSERT INTO issue_ci_advance (issue_id, head_sha)
VALUES (sqlc.arg('issue_id')::uuid, sqlc.arg('head_sha')::text)
ON CONFLICT (issue_id, head_sha) DO NOTHING;

-- name: ReleaseIssueCIAdvance :exec
DELETE FROM issue_ci_advance
WHERE issue_id = sqlc.arg('issue_id')::uuid AND head_sha = sqlc.arg('head_sha')::text;
