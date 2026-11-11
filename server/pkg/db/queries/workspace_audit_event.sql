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
