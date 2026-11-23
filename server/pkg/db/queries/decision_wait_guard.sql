-- name: ListDecisionWaitGuardIssues :many
-- Include existing episodes so a resolved wait is cleared even after assignment
-- or status changes. Property identifiers are resolved from each workspace.
SELECT i.* FROM issue i
WHERE i.metadata ?| ARRAY['decision_wait_started_at', 'decision_wait_alert_id',
                          'decision_wait_missing_request', 'decision_wait_route_error']
   OR EXISTS (SELECT 1 FROM issue_decision_park dp WHERE dp.issue_id = i.id)
   OR (i.status NOT IN ('in_review', 'done', 'cancelled')
       AND NOT EXISTS (
         SELECT 1 FROM issue_status s
         WHERE s.workspace_id = i.workspace_id AND s.key = i.status
           AND s.category IN ('done', 'closed')
       )
       AND (i.status = 'need_decision' OR EXISTS (
    SELECT 1 FROM issue_property p
    WHERE p.workspace_id = i.workspace_id AND lower(p.name) = 'readiness'
      AND p.archived_at IS NULL
      AND (i.properties ->> p.id::text = 'gated-human' OR EXISTS (
        SELECT 1 FROM jsonb_array_elements(p.config->'options') option
        WHERE option->>'name' = 'gated-human' AND i.properties ->> p.id::text = option->>'id'
      ))
)));

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

-- name: HasOpenDecisionRequestOnIssueOrAncestor :one
-- A grouped request lives on a parent (CHE-3305) or a linked issue, so an issue
-- waiting on it is covered. UNION (not UNION ALL) stops a parent cycle from recursing forever.
WITH RECURSIVE chain AS (
  SELECT i.id, i.parent_issue_id FROM issue i
  WHERE i.id = sqlc.arg('issue_id') AND i.workspace_id = sqlc.arg('workspace_id')
  UNION
  SELECT p.id, p.parent_issue_id FROM issue p JOIN chain c ON p.id = c.parent_issue_id
  WHERE p.workspace_id = sqlc.arg('workspace_id')
)
SELECT EXISTS (
  SELECT 1 FROM decision_request r
  WHERE r.authority = 'human' AND r.workspace_id = sqlc.arg('workspace_id') AND r.status = 'open'
    AND (
      r.issue_id IN (SELECT id FROM chain)
      -- A grouped request on a linked, non-ancestor issue: one issue_dependency
      -- hop from the issue or an ancestor, either direction, any type.
      OR r.issue_id IN (
        SELECT d.depends_on_issue_id FROM issue_dependency d JOIN chain ON d.issue_id = chain.id
        UNION
        SELECT d.issue_id FROM issue_dependency d JOIN chain ON d.depends_on_issue_id = chain.id
      )
    )
)::boolean;

-- name: GetDecisionWaitGuardProjectSquad :one
-- Fallback when GetDecisionWaitGuardSquad finds no single squad (CHE-4029). The
-- project's owning squad is the single active squad its lead agent leads.
-- When the issue's assignee is an agent in several squads, only the squad that
-- agent belongs to (or leads) counts.
WITH candidate AS (
  SELECT s.id FROM squad s
  JOIN issue i ON i.id = sqlc.arg('issue_id') AND i.workspace_id = sqlc.arg('workspace_id')
  JOIN project p ON p.id = i.project_id AND p.workspace_id = i.workspace_id
  WHERE s.workspace_id = i.workspace_id AND s.archived_at IS NULL
    AND p.lead_type = 'agent' AND s.leader_id = p.lead_id
    AND (i.assignee_type IS DISTINCT FROM 'agent' OR s.leader_id = i.assignee_id OR EXISTS (
      SELECT 1 FROM squad_member sm WHERE sm.squad_id = s.id AND sm.member_type = 'agent' AND sm.member_id = i.assignee_id))
)
SELECT s.* FROM squad s WHERE s.id IN (SELECT id FROM candidate) AND (SELECT count(*) FROM candidate) = 1;
