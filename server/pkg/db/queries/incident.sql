-- name: ListIncidentWorkspaces :many
-- CHE-9892: workspaces that turned on settings.incident_response (shadow or act).
SELECT id, settings FROM workspace
WHERE settings -> 'incident_response' ->> 'mode' IN ('shadow', 'act');

-- name: ListIncidentFailedRuns :many
-- Failed runs of one workspace in the last window_seconds. The caller classifies the error.
SELECT t.id, t.issue_id, t.agent_id, t.completed_at, t.error
FROM agent_task_queue t
JOIN issue i ON i.id = t.issue_id
WHERE i.workspace_id = sqlc.arg('workspace_id')
  AND t.status = 'failed'
  AND t.error IS NOT NULL
  AND t.completed_at > now() - make_interval(secs => sqlc.arg('window_seconds')::float8)
ORDER BY t.completed_at;

-- name: ListOpenIncidentIssues :many
-- One open incident issue per error class: the class and the newest failure it reports live in metadata.
SELECT i.id, (i.metadata ->> 'incident_class')::text AS class, COALESCE(i.metadata ->> 'incident_last_failure_at', '')::text AS last_failure_at
FROM issue i
WHERE i.workspace_id = sqlc.arg('workspace_id')
  AND i.metadata ->> 'incident_class' IS NOT NULL
  AND i.status NOT IN ('done', 'cancelled')
  AND NOT EXISTS (
    SELECT 1 FROM issue_status s
    WHERE s.workspace_id = i.workspace_id AND s.key = i.status AND s.category IN ('done', 'closed'))
ORDER BY i.created_at;

-- name: GetIncidentDMSession :one
-- The member's newest active DM created in the product (channel-bridged chats are out).
SELECT cs.id, cs.agent_id FROM chat_session cs
WHERE cs.workspace_id = sqlc.arg('workspace_id')
  AND cs.creator_id = sqlc.arg('creator_id')
  AND cs.status = 'active'
  AND cs.channel_origin_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM channel_chat_session_binding b WHERE b.chat_session_id = cs.id)
ORDER BY cs.updated_at DESC
LIMIT 1;

-- name: ListIncidentPendingDMs :many
-- Incident issues whose one DM has not been delivered yet: the text waits in metadata until the member has an active DM.
SELECT id, (metadata ->> 'incident_dm_text')::text AS dm_text
FROM issue
WHERE workspace_id = sqlc.arg('workspace_id')
  AND metadata ->> 'incident_dm_text' IS NOT NULL
ORDER BY created_at;
