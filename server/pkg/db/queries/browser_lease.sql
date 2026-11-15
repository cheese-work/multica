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

-- name: DeleteQueuedBrowserLeases :many
-- Revoke and auto-revoke (CHE-4734, W3-5), step 1: drop every queued lease that matches the
-- filters. A queued lease holds nothing (no epoch, no token), and the epoch check forbids an
-- ended row without an epoch, so it is deleted rather than ended. workspace_id is required;
-- every other filter is optional (NULL matches all).
DELETE FROM browser_lease
WHERE workspace_id = sqlc.arg(workspace_id)
  AND state = 'queued'
  AND (sqlc.narg(lease_id)::uuid IS NULL OR id = sqlc.narg(lease_id))
  AND (sqlc.narg(task_id)::uuid IS NULL OR task_id = sqlc.narg(task_id))
  AND (sqlc.narg(user_id)::uuid IS NULL OR user_id = sqlc.narg(user_id))
  AND (sqlc.narg(agent_id)::uuid IS NULL OR agent_id = sqlc.narg(agent_id))
  AND (sqlc.narg(profile_id)::uuid IS NULL OR profile_id = sqlc.narg(profile_id))
RETURNING *;

-- name: EndBrowserLeases :many
-- Revoke and auto-revoke, step 2 (run after step 1, so a grant that races step 1 is still
-- caught): end every active lease that matches the filters. One query serves release (lease),
-- task end and cancel (task), permission removed (user, profile, agent), plugin disabled and
-- revoke-all (workspace only). An ended lease is untouched, so a second trigger is a no-op and
-- emits nothing. An ended lease keeps holding its instance until cleanup-ack
-- (GetBrowserInstanceHolder).
UPDATE browser_lease
SET state = 'ended',
    ended_at = clock_timestamp(),
    end_reason = sqlc.arg(reason)::text
WHERE workspace_id = sqlc.arg(workspace_id)
  AND state = 'active'
  AND (sqlc.narg(lease_id)::uuid IS NULL OR id = sqlc.narg(lease_id))
  AND (sqlc.narg(task_id)::uuid IS NULL OR task_id = sqlc.narg(task_id))
  AND (sqlc.narg(user_id)::uuid IS NULL OR user_id = sqlc.narg(user_id))
  AND (sqlc.narg(agent_id)::uuid IS NULL OR agent_id = sqlc.narg(agent_id))
  AND (sqlc.narg(profile_id)::uuid IS NULL OR profile_id = sqlc.narg(profile_id))
RETURNING *;
