-- name: ListIssueLastRunStatuses :many
-- The status of an issue's most recent runs, newest first, for the weekly run
-- guard's incident line (CHE-5720).
SELECT status::text FROM agent_task_queue
WHERE issue_id = $1
ORDER BY created_at DESC, id DESC
LIMIT $2;

-- name: ClaimWeeklyRunGuardIncident :one
-- Claims the guard's incident for one (issue, day): a lost race or a repeat
-- refusal on the same day returns pgx.ErrNoRows, so one incident line is
-- written per issue and day. window_id is the ICT day.
INSERT INTO run_budget_notice (id, workspace_id, issue_id, window_id, kind)
VALUES ($1, $2, $3, $4, 'weekly_run_guard')
ON CONFLICT (issue_id, window_id, kind) DO NOTHING
RETURNING id;

-- name: ListWeeklyRunGuardIncidentsSince :many
-- The guard's incidents since a time, newest first, for the brief.
SELECT i.number::bigint AS number, a.details
FROM activity_log a JOIN issue i ON i.id = a.issue_id
WHERE a.workspace_id = sqlc.arg('workspace_id')::uuid AND a.action = 'weekly_run_guard_tripped'
  AND a.created_at >= sqlc.arg('since')::timestamptz
ORDER BY a.created_at DESC;
