-- Browser lease storage (CHE-4731, W3-2). Mint, renew and queue logic arrives in W3-3.

-- name: CreateBrowserProfile :one
INSERT INTO browser_profile (workspace_id, name, class)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetBrowserProfile :one
SELECT * FROM browser_profile
WHERE id = $1 AND workspace_id = $2;

-- name: CreateBrowserInstance :one
INSERT INTO browser_instance (workspace_id, name)
VALUES ($1, $2)
RETURNING *;

-- name: GetBrowserInstance :one
SELECT * FROM browser_instance
WHERE id = $1 AND workspace_id = $2;

-- name: CreateQueuedBrowserLease :one
INSERT INTO browser_lease (
    workspace_id, instance_id, profile_id, agent_id, task_id, issue_id, user_id,
    decision_id, class, state, expires_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'queued', $10)
RETURNING *;

-- name: GetBrowserLease :one
SELECT * FROM browser_lease
WHERE id = $1 AND workspace_id = $2;

-- name: GetBrowserInstanceHolder :one
-- The lease that holds the instance: granted, or ended and not yet cleanup-acked.
SELECT * FROM browser_lease
WHERE instance_id = $1 AND workspace_id = $2
  AND state <> 'queued' AND cleanup_acked_at IS NULL;

-- Browser instance registry (CHE-4781, W3-18). The host and class matrix is a CHECK on the table.

-- name: RegisterBrowserInstance :one
INSERT INTO browser_instance (workspace_id, name, host, gateway, allowed_classes)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (workspace_id, name) DO UPDATE
SET host = EXCLUDED.host, gateway = EXCLUDED.gateway,
    allowed_classes = EXCLUDED.allowed_classes, updated_at = clock_timestamp()
RETURNING *;

-- name: CreateBrowserGatewayCredential :one
INSERT INTO browser_gateway_credential (workspace_id, gateway, credential_hash)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetBrowserInstanceForGateway :one
-- A gateway credential resolves only to its own gateway's instances: another gateway's instance
-- returns no row, the same as a missing one.
SELECT i.* FROM browser_instance i
JOIN browser_gateway_credential c ON c.workspace_id = i.workspace_id AND c.gateway = i.gateway
WHERE c.credential_hash = $1 AND c.revoked_at IS NULL
  AND i.id = $2;

-- name: ListBrowserInstancesForGateway :many
SELECT i.* FROM browser_instance i
JOIN browser_gateway_credential c ON c.workspace_id = i.workspace_id AND c.gateway = i.gateway
WHERE c.credential_hash = $1 AND c.revoked_at IS NULL
ORDER BY i.name;
