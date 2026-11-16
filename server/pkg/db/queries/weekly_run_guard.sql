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

-- name: SumWeeklyRunGuardAllowance :one
-- Runs the issue's approved cards added to the guard's limit in the window (CHE-6172).
SELECT COALESCE(SUM(runs), 0)::bigint FROM weekly_run_guard_allowance
WHERE issue_id = $1 AND created_at > $2;

-- name: CreateWeeklyRunGuardAllowance :exec
INSERT INTO weekly_run_guard_allowance (id, workspace_id, issue_id, runs, action_id)
VALUES ($1, $2, $3, $4, $5);

-- name: LockOpenWeeklyRunGuardRefusal :one
SELECT * FROM weekly_run_guard_refusal WHERE issue_id = $1 AND state = 'open' FOR UPDATE;

-- name: InsertWeeklyRunGuardRefusal :one
-- A concurrent refusal that lost the one-open-per-issue index returns no row.
INSERT INTO weekly_run_guard_refusal (id, workspace_id, issue_id, agent_id, trigger, actor_user_id, trigger_comment_id, source_wakeup_id, source_task_id, runs)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (issue_id) WHERE state = 'open' DO NOTHING
RETURNING *;

-- name: SetWeeklyRunGuardRefusalState :exec
UPDATE weekly_run_guard_refusal SET state = $2, updated_at = now() WHERE id = $1;

-- name: ApproveWeeklyRunGuardRefusal :execrows
UPDATE weekly_run_guard_refusal SET state = 'approved', action_id = $3, updated_at = now()
WHERE id = $1 AND issue_id = $2 AND state = 'open';

-- name: ListApprovedWeeklyRunGuardRefusalsByAction :many
SELECT * FROM weekly_run_guard_refusal WHERE action_id = $1 AND state = 'approved';

-- name: CountOpenWeeklyRunGuardCards :one
SELECT count(*)::bigint FROM decision_request
WHERE issue_id = $1 AND status = 'open' AND request_fingerprint = $2;

-- name: GetWeeklyRunGuardRefusal :one
SELECT * FROM weekly_run_guard_refusal WHERE id = $1;

-- name: ListWeeklyRunGuardRefusalsToResume :many
-- Approved refusals a crashed or missed post-commit left unresumed.
SELECT * FROM weekly_run_guard_refusal WHERE state = 'approved' AND updated_at < $1;

-- name: LockApprovedWeeklyRunGuardRefusal :one
SELECT * FROM weekly_run_guard_refusal WHERE id = $1 AND state = 'approved' FOR UPDATE;

-- name: FindOpenWeeklyRunGuardCard :one
-- The issue's open weekly run guard card, for the budget read (CHE-6172).
SELECT r.id AS request_id, a.id AS action_id
FROM decision_request r JOIN decision_action a ON a.decision_request_id = r.id
WHERE r.workspace_id = $1 AND r.issue_id = $2 AND r.status = 'open' AND a.action_type = 'issue.run_guard.allow'
ORDER BY r.created_at DESC LIMIT 1;

-- name: HasRejectedWeeklyRunGuardCard :one
-- CHE-6241: the refusal's card was declined. The refusal row keeps the trigger
-- it refused; a repeat of that trigger opens no new card.
SELECT EXISTS (
    SELECT 1 FROM decision_request request
    JOIN decision_action action ON action.decision_request_id = request.id
    WHERE request.issue_id = $1 AND request.request_fingerprint = $2 AND action.state = 'rejected'
);
