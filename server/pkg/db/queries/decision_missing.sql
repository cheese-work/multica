-- name: ReconcileDecisionMissingWaits :exec
DELETE FROM decision_missing_wait d WHERE NOT EXISTS (
    SELECT 1 FROM decision_missing_candidate c
    WHERE c.workspace_id = d.workspace_id AND c.issue_id = d.issue_id
);

-- name: SeedDecisionMissingWaits :exec
INSERT INTO decision_missing_wait (workspace_id, issue_id)
SELECT workspace_id, issue_id FROM decision_missing_candidate
ON CONFLICT (workspace_id, issue_id) DO NOTHING;

-- name: ListDecisionMissingAlerts :many
SELECT d.* FROM decision_missing_wait d
JOIN decision_missing_candidate c USING (workspace_id, issue_id)
WHERE d.started_at < clock_timestamp() - interval '10 minutes' AND d.alerted_at IS NULL
ORDER BY d.checked_at NULLS FIRST, d.started_at, d.issue_id
LIMIT 100;

-- name: LockDecisionMissingWait :one
SELECT d.* FROM decision_missing_wait d
JOIN decision_missing_candidate c USING (workspace_id, issue_id)
WHERE d.workspace_id = $1 AND d.issue_id = $2
  AND d.started_at < clock_timestamp() - interval '10 minutes' AND d.alerted_at IS NULL
FOR UPDATE OF d;

-- name: StampDecisionMissingWait :exec
UPDATE decision_missing_wait SET checked_at = clock_timestamp(), comment_id = $3,
    alerted_at = CASE WHEN sqlc.arg('delivered')::bool THEN clock_timestamp() ELSE NULL END
WHERE workspace_id = $1 AND issue_id = $2;

-- name: ListMissingDecisionRequests :many
SELECT d.workspace_id, d.issue_id, c.project_id, d.started_at, d.alerted_at
FROM decision_missing_wait d JOIN decision_missing_candidate c USING (workspace_id, issue_id)
WHERE d.workspace_id = $1 AND (sqlc.narg('project_id')::uuid IS NULL OR c.project_id = sqlc.narg('project_id'))
  AND d.started_at < clock_timestamp() - interval '10 minutes'
ORDER BY d.started_at, d.issue_id;

-- name: DecisionMissingOwningSquads :many
-- Explicit assignment wins. Otherwise prefer the latest squad-attributed
-- non-chat run, then unambiguous current assignee membership. Never use an
-- unrelated member's agent ownership or a squad from a different workspace.
WITH candidates AS (
    SELECT s.id, 0 AS priority FROM issue i
    JOIN squad s ON i.assignee_type = 'squad' AND s.id = i.assignee_id AND s.workspace_id = i.workspace_id
    WHERE i.id = $2 AND i.workspace_id = $1
    UNION ALL
    SELECT t.squad_id, 1 FROM agent_task_queue t
    JOIN issue i ON i.id = t.issue_id AND i.workspace_id = $1
    WHERE i.id = $2 AND i.assignee_type IS DISTINCT FROM 'squad'
      AND t.chat_session_id IS NULL AND t.squad_id IS NOT NULL
      AND t.created_at = (SELECT max(t2.created_at) FROM agent_task_queue t2
          WHERE t2.issue_id = i.id AND t2.chat_session_id IS NULL AND t2.squad_id IS NOT NULL)
    UNION ALL
    SELECT sm.squad_id, 2 FROM issue i JOIN squad_member sm
      ON sm.member_type = i.assignee_type AND sm.member_id = i.assignee_id
    WHERE i.workspace_id = $1 AND i.id = $2 AND i.assignee_type IN ('agent', 'member')
    UNION ALL
    SELECT s.id, 2 FROM issue i JOIN squad s
      ON i.assignee_type = 'agent' AND s.leader_id = i.assignee_id AND s.workspace_id = i.workspace_id
    WHERE i.workspace_id = $1 AND i.id = $2
), best AS (SELECT min(priority) AS priority FROM candidates)
SELECT DISTINCT s.* FROM candidates c JOIN best USING (priority)
JOIN squad s ON s.id = c.id AND s.workspace_id = $1
WHERE s.archived_at IS NULL;
