-- name: SumIssueTokens :one
-- All tokens of every run of an issue (input, output, cache read and write, as the effreport counts them), for the stop-loss (CHE-9340).
SELECT COALESCE(SUM(u.input_tokens + u.output_tokens + u.cache_read_tokens + u.cache_write_tokens), 0)::bigint
FROM task_usage u JOIN agent_task_queue t ON t.id = u.task_id
WHERE t.issue_id = $1;

-- name: CountIssueLivePullRequests :one
-- Linked PRs that are open, draft or merged: the delivery that ends the stop-loss.
SELECT COUNT(*)::bigint
FROM issue_pull_request ip JOIN github_pull_request p ON p.id = ip.pull_request_id
WHERE ip.issue_id = $1 AND p.state IN ('open', 'draft', 'merged');

-- name: CountOpenChildIssues :one
-- Children that are not finished, for the stop-loss exclusion of a plan that waits on them.
SELECT COUNT(*)::bigint FROM issue child
WHERE child.parent_issue_id = $1 AND child.status NOT IN ('done', 'cancelled')
  AND NOT EXISTS (
    SELECT 1 FROM issue_status s
    WHERE s.workspace_id = child.workspace_id AND s.key = child.status AND s.category IN ('done', 'closed'));

-- name: SumWeeklyRunGuardAllowanceEver :one
-- Runs the issue's approved cards ever added: the stop-loss counts the issue's whole life.
SELECT COALESCE(SUM(runs), 0)::bigint FROM weekly_run_guard_allowance WHERE issue_id = $1;
