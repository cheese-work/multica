-- name: ListDecisionWaitGuardIssues :many
-- Include existing episodes so a resolved wait is cleared even after assignment
-- or status changes. Property identifiers are resolved from each workspace.
SELECT i.* FROM issue i
JOIN workspace w ON w.id = i.workspace_id
WHERE i.status = 'need_decision' OR i.metadata ? 'decision_wait_started_at'
   OR EXISTS (
    SELECT 1 FROM issue_property p
    WHERE p.workspace_id = i.workspace_id AND lower(p.name) = 'readiness'
      AND p.archived_at IS NULL
      AND (i.properties ->> p.id::text = 'gated-human' OR EXISTS (
        SELECT 1 FROM jsonb_array_elements(p.config->'options') option
        WHERE option->>'name' = 'gated-human' AND i.properties ->> p.id::text = option->>'id'
      ))
);

-- name: GetDecisionWaitGuardSquad :one
-- A current squad assignment wins. Member assignment preserves the latest
-- owning leader task's scoped squad provenance. Membership is a last resort
-- only when the assigned agent belongs to exactly one active squad.
SELECT s.* FROM squad s
WHERE s.workspace_id = sqlc.arg('workspace_id') AND s.archived_at IS NULL
  AND s.id = COALESCE(
    (SELECT i.assignee_id FROM issue i WHERE i.id = sqlc.arg('issue_id')
      AND i.workspace_id = sqlc.arg('workspace_id') AND i.assignee_type = 'squad'),
    (SELECT t.squad_id FROM agent_task_queue t
      JOIN agent a ON a.id = t.agent_id AND a.workspace_id = sqlc.arg('workspace_id')
      JOIN squad owner ON owner.id = t.squad_id AND owner.workspace_id = sqlc.arg('workspace_id') AND owner.archived_at IS NULL
      WHERE t.issue_id = sqlc.arg('issue_id') AND t.is_leader_task
      ORDER BY t.created_at DESC, t.id DESC LIMIT 1),
    (SELECT min(sm.squad_id::text)::uuid FROM squad_member sm
      JOIN squad candidate ON candidate.id = sm.squad_id AND candidate.workspace_id = sqlc.arg('workspace_id') AND candidate.archived_at IS NULL
      JOIN issue i ON i.id = sqlc.arg('issue_id') AND i.workspace_id = sqlc.arg('workspace_id')
      WHERE i.assignee_type = 'agent' AND sm.member_type = 'agent' AND sm.member_id = i.assignee_id
      HAVING count(*) = 1)
  );
