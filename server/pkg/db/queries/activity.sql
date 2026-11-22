-- name: ListActivitiesForIssue :many
-- The NEWEST $2 activities for an issue, returned in chronological order.
--
-- The cap has to bite at the OLD end, not the new one. The inner query takes
-- the window with the keyset ordering (created_at DESC, id DESC), which
-- idx_activity_log_issue_keyset (migration 068) satisfies without a sort step —
-- it is not an index-only scan, since the index does not cover the columns
-- SELECT * needs, so the heap is still read for the rows in the window. The
-- outer query re-sorts ascending so every caller keeps the chronological
-- contract it already had.
--
-- Capping with ORDER BY created_at ASC instead discards the newest rows, which
-- made a busy issue's timeline appear to stop at some point in the past with no
-- indication anything was missing. Activity is machine-paced (description
-- autosave, every agent run, status/assignee changes), so this was reachable in
-- normal use, not only on pathological issues (MUL-5492).
SELECT * FROM (
    SELECT * FROM activity_log
    WHERE issue_id = $1
    ORDER BY created_at DESC, id DESC
    LIMIT $2
) AS recent
ORDER BY created_at ASC, id ASC;

-- name: GetActivity :one
SELECT * FROM activity_log
WHERE id = $1;

-- name: CreateActivity :one
INSERT INTO activity_log (
    workspace_id, issue_id, actor_type, actor_id, action, details, id, created_at
) VALUES ($1, $2, $3, $4, $5, $6, COALESCE(sqlc.narg('id')::uuid, gen_random_uuid()), COALESCE(sqlc.narg('created_at')::timestamptz, now()))
RETURNING *;

-- name: HasSquadLeaderNoActionEvaluationForTask :one
SELECT EXISTS (
  SELECT 1
  FROM activity_log
  WHERE issue_id = @issue_id
    AND actor_type = 'agent'
    AND actor_id = @agent_id
    AND action = 'squad_leader_evaluated'
    AND details->>'outcome' = 'no_action'
    AND details->>'task_id' = @task_id::text
) AS exists;

-- name: CountAssigneeChangesByActor :many
-- Count how many times a user assigned each target via assignee_changed activities.
SELECT
  details->>'to_type' as assignee_type,
  details->>'to_id' as assignee_id,
  COUNT(*)::bigint as frequency
FROM activity_log
WHERE workspace_id = $1
  AND actor_id = $2
  AND actor_type = 'member'
  AND action = 'assignee_changed'
  AND details->>'to_type' IS NOT NULL
  AND details->>'to_id' IS NOT NULL
GROUP BY details->>'to_type', details->>'to_id';

-- name: GetLastIssueStatusChange :one
-- Who last changed an issue's status, and to what: names the mover when an
-- agent's status change is refused because the issue left its step (MUL-7420).
SELECT actor_type, actor_id FROM activity_log
WHERE issue_id = @issue_id AND action = 'status_changed'
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: CountEntryGateWarnings :one
-- Entry-gate warnings already recorded for an issue and step: those for this
-- PR head, and all of them. The write holds the issue row lock, so the pair
-- decides, without a table of its own, whether this warning is new and
-- whether it is the step's first (CHE-2295).
SELECT (count(*) FILTER (WHERE details->>'head' = sqlc.arg('head')::text))::bigint AS same_head,
       count(*)::bigint AS total
FROM activity_log
WHERE issue_id = sqlc.arg('issue_id')::uuid
  AND action = 'entry_gate_warning'
  AND details->>'step' = sqlc.arg('step')::text;

-- name: ListIssueRunSkips :many
-- The runs the platform declined to start on an issue (CHE-4649), newest
-- first. They are no task rows, so the run list reads them from the activity.
SELECT id, created_at, details FROM activity_log
WHERE issue_id = $1 AND action = 'run_skipped'
ORDER BY created_at DESC
LIMIT 50;

-- name: ListIssueAuthorTasks :many
-- Every agent task on an issue that counts as authorship (CHE-7658 step 11):
-- all of them except a task whose every workflow_role row is a reviewer role or
-- a step after the work (a review, a decision, a merge, an acceptance, a block)
-- or no step at all (a wakeup run; CHE-9002). A task with no row counts (fail
-- closed), and so does a row at any other step, such as todo or in_progress. So does
-- any task of the agent that authored a linked PR, whatever its step: the task
-- whose key ends the PR branch, or one that ran on that branch before the PR
-- opened (same match as ListGitHubReviewAuthorCandidates). The lab inputs come from the
-- task row: the model the daemon applied and its runtime's provider.
SELECT t.id AS task_id, t.agent_id, COALESCE(r.provider, '')::text AS provider, COALESCE(t.applied_model, '')::text AS model
FROM agent_task_queue t
LEFT JOIN agent_runtime r ON r.id = t.runtime_id
WHERE t.issue_id = @issue_id
  AND NOT (
    EXISTS (SELECT 1 FROM activity_log a WHERE a.issue_id = t.issue_id AND a.action = 'workflow_role'
              AND a.details->>'task_id' = t.id::text)
    AND NOT EXISTS (SELECT 1 FROM activity_log a WHERE a.issue_id = t.issue_id AND a.action = 'workflow_role'
              AND a.details->>'task_id' = t.id::text
              AND a.details->>'role' IS DISTINCT FROM 'reviewer'
              AND COALESCE(a.details->>'status', '') NOT IN ('', 'agent_review', 'in_review', 'need_decision', 'merged', 'approved', 'agent_accepted', 'blocked'))
    AND NOT EXISTS (
      SELECT 1 FROM issue_pull_request l JOIN github_pull_request p ON p.id = l.pull_request_id
      WHERE l.issue_id = t.issue_id AND p.branch IS NOT NULL AND p.branch <> ''
        AND (right(p.branch, 12) = right(replace(t.id::text, '-', ''), 12)
             OR (t.branch_name = p.branch AND t.created_at <= p.pr_created_at)))
  )
ORDER BY t.created_at, t.id;

-- name: CopyWorkflowRoleToRetry :exec
-- A retry works the role its parent did: the newest workflow_role row of the
-- parent task is copied for the child, so a reviewer's retry stays a reviewer's.
INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details)
SELECT a.workspace_id, a.issue_id, a.actor_type, a.actor_id, 'workflow_role',
       jsonb_set(a.details, '{task_id}', to_jsonb(@child_task_id::text))
FROM activity_log a
WHERE a.issue_id = @issue_id AND a.action = 'workflow_role' AND a.details->>'task_id' = @parent_task_id::text
ORDER BY a.created_at DESC, a.id DESC
LIMIT 1;

-- name: GetLatestChainedRunRetryDetails :one
-- The newest chained_run_retry row of an issue: the retry a failed chained run
-- got (CHE-7675). pgx.ErrNoRows when the issue never had one.
SELECT details FROM activity_log
WHERE issue_id = $1 AND action = 'chained_run_retry'
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: HasChainedRunEntered :one
-- Whether the task carried the issue into the step itself (a chained move,
-- CHE-7675), as opposed to being dispatched straight into it.
SELECT EXISTS (
  SELECT 1 FROM activity_log
  WHERE issue_id = @issue_id AND action = 'chained_run_entered'
    AND details->>'task_id' = @task_id::text AND details->>'step' = @step::text
);

-- name: GetLatestIssueActivityByAction :one
-- The newest activity of one action on an issue, however old: a scan window
-- would miss a quick_entry row that a busy issue has long buried.
SELECT * FROM activity_log
WHERE issue_id = $1 AND action = $2
ORDER BY created_at DESC, id DESC
LIMIT 1;
