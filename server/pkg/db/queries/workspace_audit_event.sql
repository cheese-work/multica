-- name: InsertWorkspaceAuditEvent :exec
-- Callers run this inside the transaction of the mutation it describes. There
-- is deliberately no UPDATE or DELETE statement for this table in this package.
INSERT INTO workspace_audit_event (
    id, workspace_id, actor_type, actor_id, task_id, actor_source,
    kind, target_type, target_id, "before", "after", external_requested
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12
);

-- name: AuditWorkspaceDelete :one
-- Workspace deletion only: sets the purge flag inside the function, in the
-- caller's transaction, and returns the number of rows removed.
SELECT audit_workspace_delete($1)::bigint;

-- name: ListWorkspaceAuditEvents :many
-- Read side (CHE-5644). Keyset page on (at DESC, id DESC), served by
-- idx_workspace_audit_event_workspace_at. The cursor row is excluded.
SELECT id, at, actor_type, actor_id, task_id, kind, target_type, target_id,
       "before", "after", external_requested
FROM workspace_audit_event
WHERE workspace_id = @workspace_id
  AND (sqlc.narg('kind_prefix')::text IS NULL OR starts_with(kind, sqlc.narg('kind_prefix')::text))
  AND (sqlc.narg('target_id')::uuid IS NULL OR target_id = sqlc.narg('target_id')::uuid)
  AND (sqlc.narg('actor_id')::uuid IS NULL OR actor_id = sqlc.narg('actor_id')::uuid)
  AND (sqlc.narg('since')::timestamptz IS NULL OR at >= sqlc.narg('since')::timestamptz)
  AND (sqlc.narg('until')::timestamptz IS NULL OR at < sqlc.narg('until')::timestamptz)
  AND (sqlc.narg('cursor_at')::timestamptz IS NULL
       OR (at, id) < (sqlc.narg('cursor_at')::timestamptz, sqlc.narg('cursor_id')::uuid))
ORDER BY at DESC, id DESC
LIMIT @row_limit;

-- name: ResolveAuditNames :many
-- Display names for the actors and targets of one audit page, in one query.
-- kind 'user' = a member actor (actor_id is the user id); the others are the
-- target types of audit rows. Workspace-scoped tables filter by workspace.
SELECT 'user'::text AS kind, u.id, u.name FROM "user" u WHERE u.id = ANY(@user_ids::uuid[])
UNION ALL SELECT 'agent', a.id, a.name FROM agent a WHERE a.workspace_id = @workspace_id AND a.id = ANY(@ids::uuid[])
UNION ALL SELECT 'runtime', r.id, r.name FROM agent_runtime r WHERE r.workspace_id = @workspace_id AND r.id = ANY(@ids::uuid[])
UNION ALL SELECT 'squad', s.id, s.name FROM squad s WHERE s.workspace_id = @workspace_id AND s.id = ANY(@ids::uuid[])
UNION ALL SELECT 'skill', k.id, k.name FROM skill k WHERE k.workspace_id = @workspace_id AND k.id = ANY(@ids::uuid[])
UNION ALL SELECT 'project', p.id, p.title FROM project p WHERE p.workspace_id = @workspace_id AND p.id = ANY(@ids::uuid[])
UNION ALL SELECT 'autopilot', ap.id, ap.title FROM autopilot ap WHERE ap.workspace_id = @workspace_id AND ap.id = ANY(@ids::uuid[])
UNION ALL SELECT 'workspace', w.id, w.name FROM workspace w WHERE w.id = @workspace_id AND w.id = ANY(@ids::uuid[])
UNION ALL SELECT 'member', m.id, u.name FROM member m JOIN "user" u ON u.id = m.user_id WHERE m.workspace_id = @workspace_id AND m.id = ANY(@ids::uuid[]);
