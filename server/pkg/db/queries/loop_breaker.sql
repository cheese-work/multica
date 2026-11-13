-- name: CountAgentIssueRunsSince :one
-- Runs an agent started on one issue since a cutoff (CHE-5135 loop breaker).
-- A run cancelled before it started is not a run.
SELECT count(*)::bigint FROM agent_task_queue
WHERE issue_id = $1 AND agent_id = $2 AND created_at > $3
  AND (status <> 'cancelled' OR started_at IS NOT NULL);

-- name: ListAgentIssueRunTriggersSince :many
-- The trigger type of an agent's most recent runs on one issue since a cutoff,
-- newest first, for the loop breaker's alert.
SELECT COALESCE(
    NULLIF(trigger_evidence_kind, ''),
    CASE WHEN trigger_comment_id IS NOT NULL THEN 'comment'
         WHEN autopilot_run_id IS NOT NULL THEN 'autopilot_run'
         ELSE 'unknown' END
)::text AS trigger_type
FROM agent_task_queue
WHERE issue_id = $1 AND agent_id = $2 AND created_at > $3
  AND (status <> 'cancelled' OR started_at IS NOT NULL)
ORDER BY created_at DESC, id DESC
LIMIT $4;

-- name: ClaimLoopBreakerNotice :one
-- Claims the loop-breaker alert for one (issue, agent): none if one was claimed
-- within the last hour. A lost race returns pgx.ErrNoRows, so only the claiming
-- caller posts the comment. window_id is "<agent id>:<claim time>".
INSERT INTO run_budget_notice (id, workspace_id, issue_id, window_id, kind)
SELECT $1, $2, $3, $4::text, 'loop_breaker'
WHERE NOT EXISTS (
    SELECT 1 FROM run_budget_notice
    WHERE issue_id = $3 AND kind = 'loop_breaker'
      AND window_id LIKE $5::text AND created_at > now() - interval '1 hour'
)
ON CONFLICT (issue_id, window_id, kind) DO NOTHING
RETURNING id;
