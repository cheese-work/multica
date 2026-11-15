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

-- name: LockBrowserInstance :one
-- Serializes cleanup-ack, quarantine and grant on one instance (CHE-4736, W3-7).
SELECT * FROM browser_instance
WHERE id = $1 AND workspace_id = $2
FOR UPDATE;

-- name: SetBrowserInstancePhase :one
UPDATE browser_instance
SET phase = $3, updated_at = clock_timestamp()
WHERE id = $1 AND workspace_id = $2
RETURNING *;

-- name: AckBrowserLeaseCleanup :one
-- Stamps cleanup-ack on the ended lease that still holds the instance, for the given epoch only.
UPDATE browser_lease
SET cleanup_acked_at = clock_timestamp()
WHERE instance_id = $1 AND workspace_id = $2 AND epoch = $3
  AND state = 'ended' AND cleanup_acked_at IS NULL
RETURNING *;
