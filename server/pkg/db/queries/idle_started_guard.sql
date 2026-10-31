-- name: ListIdleStartedGuardWorkspaces :many
SELECT id, settings FROM workspace
WHERE settings -> 'idle_started_guard' ->> 'mode' IN ('shadow', 'act');

-- name: ListIdleStartedGuardIssues :many
-- Main scan. idle_since is the newest human or agent signal on the issue; system
-- comments, system activity and metadata writes never count, so the guard's own
-- writes cannot reset the clock. Issues that already carry an idle_guard episode
-- are skipped. Any agent-owned continuation excludes the issue. issue_id narrows
-- the scan to one issue: act mode re-runs it under the issue lock. episode keeps
-- an issue that carries an idle_guard receipt, for re-checking its own episode.
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
  AND (sqlc.narg('episode')::bool IS TRUE OR NOT i.metadata ? 'idle_guard')
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
-- Issues with an idle_guard receipt that need another look: a notice owed, a
-- refused move to retry, or an issue that left the episode (a repair). A settled
-- receipt on an issue still where it settled is left alone.
SELECT i.id, i.workspace_id, i.status, (i.metadata -> 'idle_guard')::jsonb AS receipt
FROM issue i
WHERE i.workspace_id = sqlc.arg('workspace_id')
  AND i.metadata ? 'idle_guard'
  AND NOT (
    i.status = i.metadata -> 'idle_guard' ->> 'to'
    AND i.metadata -> 'idle_guard' ->> 'state' IN ('notified', 'gave_up', 'resumed')
    AND COALESCE(i.metadata -> 'idle_guard' ->> 'moved_at', '') <> '')
  AND NOT (
    i.status = i.metadata -> 'idle_guard' ->> 'from'
    AND i.metadata -> 'idle_guard' ->> 'state' = 'gave_up'
    AND COALESCE(i.metadata -> 'idle_guard' ->> 'moved_at', '') = '')
ORDER BY i.id;
