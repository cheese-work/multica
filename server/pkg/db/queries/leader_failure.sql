-- name: LockIssuesForLeaderFailureMarker :exec
SELECT id FROM issue
WHERE id = ANY($1::uuid[])
ORDER BY id
FOR NO KEY UPDATE;

-- name: SetSquadLeaderFailureMarkers :exec
UPDATE issue i SET
    metadata = jsonb_set(i.metadata, '{squad_leader_failed}', 'true'),
    revision = i.revision + 1,
    updated_at = now()
WHERE i.assignee_type = 'squad'
  AND i.metadata -> 'squad_leader_failed' IS DISTINCT FROM 'true'::jsonb
  AND EXISTS (
      SELECT 1 FROM agent_task_queue failed
      JOIN squad s ON s.id = i.assignee_id AND s.workspace_id = i.workspace_id
      WHERE failed.id = ANY($1::uuid[])
        AND failed.issue_id = i.id
        AND failed.agent_id = s.leader_id
        AND failed.status = 'failed'
        AND NOT EXISTS (
            SELECT 1 FROM agent_task_queue successor
            WHERE successor.issue_id = i.id
              AND (successor.status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')
                   OR successor.started_at > failed.completed_at)
        )
  );

-- name: ClearSquadLeaderFailureMarker :exec
UPDATE issue SET
    metadata = metadata - 'squad_leader_failed',
    revision = revision + 1,
    updated_at = now()
WHERE id = $1 AND metadata ? 'squad_leader_failed';
