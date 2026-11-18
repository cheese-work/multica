-- name: ListBlockedEntryLinkCandidates :many
-- CHE-6973: the owner's enabled wakeups on the issue that could carry a blocked
-- entry: no last_error, a deadline within 7 days, and a run on timeout. The
-- caller compares each condition with metadata.blocked_on.
SELECT * FROM issue_wakeup
WHERE issue_id = $1 AND agent_id = $2 AND enabled
  AND last_error IS NULL AND on_timeout = 'wake'
  AND expires_at IS NOT NULL AND expires_at > now() AND expires_at <= now() + interval '7 days'
ORDER BY created_at, id;

-- name: ListBlockedEntryLinkedIssues :many
-- CHE-6973: blocked issues whose entry run was skipped for a linked wakeup.
SELECT * FROM issue
WHERE status = 'blocked' AND metadata ? 'blocked_entry_link'
ORDER BY updated_at
LIMIT 200;

-- name: WakeupHasRun :one
SELECT EXISTS (SELECT 1 FROM agent_task_queue WHERE context->>'wakeup_id' = sqlc.arg('wakeup_id')::text) AS has_run;
