-- name: ListIdleStartedGuardWorkspaces :many
SELECT id, settings FROM workspace
WHERE settings -> 'idle_started_guard' ->> 'mode' IN ('shadow', 'act');

-- name: ListIdleStartedGuardIssues :many
-- Main scan. idle_since is the newest human or agent signal on the issue; system
-- comments, system activity and metadata writes never count, so the guard's own
-- writes cannot reset the clock. Issues that already carry an issue_idle_guard episode
-- are skipped. Any agent-owned continuation excludes the issue. issue_id narrows
-- the scan to one issue: act mode re-runs it under the issue lock. episode keeps
-- an issue that carries an issue_idle_guard receipt, for re-checking its own episode.
SELECT i.id, i.workspace_id, i.status, clock.idle_since::timestamptz AS idle_since
FROM issue i
CROSS JOIN LATERAL (
  SELECT GREATEST(
    i.created_at,
    (SELECT max(c.created_at) FROM comment c
      WHERE c.issue_id = i.id AND c.deleted_at IS NULL AND c.author_type <> 'system'),
    (SELECT max(GREATEST(t.created_at, t.completed_at)) FROM agent_task_queue t WHERE t.issue_id = i.id),
    (SELECT max(a.created_at) FROM activity_log a
      WHERE a.issue_id = i.id AND a.actor_type IN ('member', 'agent'))
  ) AS idle_since
) clock
WHERE i.workspace_id = sqlc.arg('workspace_id')
  AND (sqlc.narg('issue_id')::uuid IS NULL OR i.id = sqlc.narg('issue_id')::uuid)
  AND i.status = ANY (sqlc.arg('statuses')::text[])
  AND (sqlc.narg('episode')::bool IS TRUE OR NOT EXISTS (SELECT 1 FROM issue_idle_guard g WHERE g.issue_id = i.id))
  AND clock.idle_since <= now() - make_interval(secs => sqlc.arg('after_seconds')::float8)
  AND NOT EXISTS (
    SELECT 1 FROM agent_task_queue t
    WHERE t.issue_id = i.id AND t.status IN ('queued', 'dispatched', 'running', 'waiting_local_directory'))
  AND NOT EXISTS (
    SELECT 1 FROM issue_wakeup w
    WHERE w.issue_id = i.id AND w.enabled AND w.paused_reason IS NULL
      AND (
        (w.system_rule IS NULL AND w.kind IN ('at', 'every', 'cron') AND w.next_fire_at IS NOT NULL)
        OR (w.system_rule IS NULL AND w.kind = 'event' AND w.condition IS NULL
            AND (w.filter_actor_type = 'agent'
                 OR (w.filter_actor_type IS NULL
                     AND EXISTS (SELECT 1 FROM unnest(w.event_types) e WHERE e NOT LIKE 'comment.%'))))
        OR (w.system_rule IS NULL AND w.condition ->> 'type' IN ('children_done', 'pull_request', 'github_run', 'other_issue'))
        OR (w.system_rule LIKE 'pr\_%' AND EXISTS (
              SELECT 1 FROM issue_pull_request ip JOIN github_pull_request p ON p.id = ip.pull_request_id
              WHERE ip.issue_id = i.id AND p.state IN ('open', 'draft')))
      ))
  AND NOT EXISTS (
    SELECT 1 FROM issue child
    WHERE child.parent_issue_id = i.id AND child.status NOT IN ('done', 'cancelled')
      AND NOT EXISTS (
        SELECT 1 FROM issue_status s
        WHERE s.workspace_id = child.workspace_id AND s.key = child.status AND s.category IN ('done', 'closed')))
  AND NOT EXISTS (
    SELECT 1 FROM issue_pull_request ip JOIN github_pull_request p ON p.id = ip.pull_request_id
    WHERE ip.issue_id = i.id AND p.state IN ('open', 'draft') AND p.checks_rollup_state = 'PENDING')
ORDER BY clock.idle_since, i.id;

-- name: IdleStartedGuardHumanWait :one
-- Human-wait evidence: an enabled member-filtered event wakeup, or a newest
-- non-system comment that an agent wrote to a member by mention.
SELECT (
  EXISTS (
    SELECT 1 FROM issue_wakeup w
    WHERE w.issue_id = sqlc.arg('issue_id') AND w.enabled AND w.paused_reason IS NULL
      AND w.system_rule IS NULL AND w.kind = 'event' AND w.filter_actor_type = 'member')
  OR COALESCE((
    SELECT c.author_type = 'agent' AND c.content LIKE '%mention://member/%'
    FROM comment c
    WHERE c.issue_id = sqlc.arg('issue_id') AND c.deleted_at IS NULL AND c.author_type <> 'system'
    ORDER BY c.created_at DESC, c.id DESC LIMIT 1), false)
)::bool AS waiting;

-- name: ListIdleStartedGuardEpisodes :many
-- Issues with an issue_idle_guard receipt that need another look: a notice owed, a
-- refused move to retry, or an issue that left the episode (a repair). A settled
-- receipt on an issue still where it settled is left alone. A move the guard
-- gave up on is settled until a person or an agent acts on the issue again: the
-- idle clock, not counting the alert task the guard queued itself, is then past
-- the idle time the receipt recorded.
SELECT i.id, i.workspace_id, i.status, sqlc.embed(g)
FROM issue i
JOIN issue_idle_guard g ON g.issue_id = i.id
CROSS JOIN LATERAL (
  SELECT GREATEST(
    i.created_at,
    (SELECT max(c.created_at) FROM comment c
      WHERE c.issue_id = i.id AND c.deleted_at IS NULL AND c.author_type <> 'system'),
    (SELECT max(GREATEST(t.created_at, t.completed_at)) FROM agent_task_queue t
      WHERE t.issue_id = i.id AND t.id::text IS DISTINCT FROM g.alert_id::text),
    (SELECT max(a.created_at) FROM activity_log a
      WHERE a.issue_id = i.id AND a.actor_type IN ('member', 'agent'))
  ) AS idle_since
) clock
WHERE i.workspace_id = sqlc.arg('workspace_id')
  AND NOT (
    i.status = g.to_status
    AND g.state IN ('notified', 'gave_up', 'resumed')
    AND g.moved_at IS NOT NULL)
  AND NOT (
    i.status = g.from_status
    AND g.state = 'gave_up'
    AND g.moved_at IS NULL
    AND clock.idle_since <= g.idle_since)
ORDER BY i.id;

-- name: GetIssueIdleGuard :one
SELECT * FROM issue_idle_guard WHERE workspace_id = $1 AND issue_id = $2;

-- name: UpsertIssueIdleGuard :exec
INSERT INTO issue_idle_guard (issue_id, workspace_id, state, kind, from_status, to_status, idle_since, moved_at,
                              attempts, alert_id, comment_id, error, alert_attempts, alert_at, alert_error)
VALUES (sqlc.arg('issue_id'), sqlc.arg('workspace_id'), sqlc.arg('state'), sqlc.arg('kind'), sqlc.arg('from_status'),
        sqlc.arg('to_status'), sqlc.arg('idle_since'), sqlc.arg('moved_at'), sqlc.arg('attempts'), sqlc.arg('alert_id'),
        sqlc.arg('comment_id'), sqlc.arg('error'), sqlc.arg('alert_attempts'), sqlc.arg('alert_at'), sqlc.arg('alert_error'))
ON CONFLICT (issue_id) DO UPDATE SET
    state = EXCLUDED.state, kind = EXCLUDED.kind, from_status = EXCLUDED.from_status, to_status = EXCLUDED.to_status,
    idle_since = EXCLUDED.idle_since, moved_at = EXCLUDED.moved_at, attempts = EXCLUDED.attempts,
    alert_id = EXCLUDED.alert_id, comment_id = EXCLUDED.comment_id, error = EXCLUDED.error,
    alert_attempts = EXCLUDED.alert_attempts, alert_at = EXCLUDED.alert_at, alert_error = EXCLUDED.alert_error;

-- name: DeleteIssueIdleGuard :exec
DELETE FROM issue_idle_guard WHERE workspace_id = $1 AND issue_id = $2;
