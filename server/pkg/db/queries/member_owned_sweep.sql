-- name: ListMemberOwnedSweepIssues :many
-- CHE-5838. Open issues assigned to a member with no open human decision
-- request, each with the last agent that ran on it. No agent run starts on a
-- member-owned issue, so without an open card nobody is waiting on the member
-- and nobody is told. An issue no agent ever ran on has no last agent and is
-- not listed. issue_id narrows the scan to one issue: the sweep re-runs it
-- under the issue lock.
SELECT i.id, i.workspace_id, last.agent_id
FROM issue i
CROSS JOIN LATERAL (
  SELECT t.agent_id
  FROM agent_task_queue t
  JOIN agent a ON a.id = t.agent_id AND a.archived_at IS NULL
  WHERE t.issue_id = i.id
  ORDER BY GREATEST(t.created_at, t.completed_at) DESC NULLS LAST, t.id DESC
  LIMIT 1
) last
WHERE i.assignee_type = 'member'
  AND (sqlc.narg('issue_id')::uuid IS NULL OR i.id = sqlc.narg('issue_id')::uuid)
  AND i.status NOT IN ('done', 'cancelled')
  AND NOT EXISTS (
    SELECT 1 FROM issue_status s
    WHERE s.workspace_id = i.workspace_id AND s.key = i.status AND s.category IN ('done', 'closed'))
  AND NOT EXISTS (
    SELECT 1 FROM decision_request d
    WHERE d.authority = 'human' AND d.workspace_id = i.workspace_id AND d.issue_id = i.id AND d.status = 'open')
ORDER BY i.created_at, i.id;
