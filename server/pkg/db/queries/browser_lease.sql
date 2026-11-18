-- Browser lease storage (CHE-4731, W3-2). Mint, renew and queue logic arrives in W3-3.

-- name: CreateBrowserProfile :one
-- hosts are the site hosts a login profile reaches, recorded once at creation (CHE-4782, W3-19).
INSERT INTO browser_profile (workspace_id, name, class, hosts)
VALUES (sqlc.arg(workspace_id), sqlc.arg(name), sqlc.arg(class), COALESCE(sqlc.narg(hosts)::text[], '{}'))
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

-- name: AckBrowserLeaseCleanup :one
-- Stamps cleanup-ack on the ended lease that still holds the instance, for the given epoch only.
UPDATE browser_lease
SET cleanup_acked_at = clock_timestamp()
WHERE instance_id = $1 AND workspace_id = $2 AND epoch = $3
  AND state = 'ended' AND cleanup_acked_at IS NULL
RETURNING *;

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

-- Mint, renew and queue API (CHE-4732, W3-3).

-- name: ListBrowserInstances :many
SELECT * FROM browser_instance
WHERE workspace_id = $1
ORDER BY name, id;

-- name: LockBrowserLeaseQueue :exec
-- Serializes queue decisions for one (workspace, class). The grant itself is made atomic by
-- ClaimIdleBrowserInstance, so S2 and H, which share the C00 instance, stay safe.
SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0));

-- name: ClaimIdleBrowserInstance :one
-- Grants the instance: raises the fence epoch and leaves idle_clean. No row when the instance
-- is not idle_clean or a lease still holds it.
UPDATE browser_instance bi
SET epoch = bi.epoch + 1, phase = 'starting', updated_at = clock_timestamp()
WHERE bi.id = $1 AND bi.workspace_id = $2 AND bi.phase = 'idle_clean'
  AND NOT EXISTS (
      SELECT 1 FROM browser_lease l
      WHERE l.instance_id = bi.id AND l.state <> 'queued' AND l.cleanup_acked_at IS NULL
  )
RETURNING *;

-- name: SetBrowserInstancePhase :exec
UPDATE browser_instance
SET phase = $3, updated_at = clock_timestamp()
WHERE id = $1 AND workspace_id = $2;

-- name: GetOpenBrowserLeaseForTask :one
SELECT * FROM browser_lease
WHERE workspace_id = $1 AND task_id = $2 AND state <> 'ended'
ORDER BY created_at DESC
LIMIT 1;

-- name: GetBrowserLeaseForTask :one
SELECT * FROM browser_lease
WHERE id = $1 AND workspace_id = $2 AND task_id = $3;

-- name: HeadQueuedBrowserLease :one
SELECT * FROM browser_lease
WHERE workspace_id = $1 AND class = $2 AND state = 'queued'
ORDER BY created_at, id
LIMIT 1;

-- name: CountBrowserLeasesAhead :one
-- Queue position of a queued lease, first in first out: 1 is next.
SELECT count(*)::bigint FROM browser_lease l, browser_lease me
WHERE me.id = $1 AND me.state = 'queued'
  AND l.workspace_id = me.workspace_id AND l.class = me.class AND l.state = 'queued'
  AND (l.created_at, l.id) <= (me.created_at, me.id);

-- name: ActivateBrowserLease :one
-- A queued lease takes the instance. A queued row carries a placeholder instance_id and the
-- hard limit counts from the grant, not from the wait.
UPDATE browser_lease
SET state = 'active', instance_id = $3, epoch = $4, granted_at = $5, expires_at = $6
WHERE id = $1 AND workspace_id = $2 AND state = 'queued'
RETURNING *;

-- name: DeleteQueuedBrowserLease :execrows
-- A queued lease never held an instance and has no epoch, so it cannot be 'ended' (check
-- browser_lease_epoch_iff_granted); leaving the queue deletes the row.
DELETE FROM browser_lease
WHERE id = $1 AND workspace_id = $2 AND state = 'queued';

-- name: SetBrowserLeaseJTI :exec
UPDATE browser_lease SET jti_hash = $3
WHERE id = $1 AND workspace_id = $2 AND state = 'active';

-- name: ShortenBrowserLeaseExpiry :one
-- Renew revalidated a decision: the limit only ever moves earlier, never past the hard limit
-- set at grant. RETURNING gives the persisted minimum, which is the only deadline a token may
-- be signed with; no row means the lease is no longer active.
UPDATE browser_lease SET expires_at = LEAST(expires_at, $3)
WHERE id = $1 AND workspace_id = $2 AND state = 'active'
RETURNING *;

-- name: EndBrowserLease :one
-- The instance stays held until cleanup-ack (W3-7).
UPDATE browser_lease
SET state = 'ended', ended_at = $3, end_reason = $4
WHERE id = $1 AND workspace_id = $2 AND state = 'active'
RETURNING *;

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
  AND (sqlc.narg(decision_id)::uuid IS NULL OR decision_id = sqlc.narg(decision_id))
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
  AND (sqlc.narg(decision_id)::uuid IS NULL OR decision_id = sqlc.narg(decision_id))
RETURNING *;

-- Class H login lease held by a member (CHE-5989, W3-3b).

-- name: GetOpenBrowserLoginLeaseForUser :one
SELECT * FROM browser_lease
WHERE workspace_id = $1 AND user_id = $2 AND class = 'H' AND state <> 'ended'
ORDER BY created_at DESC
LIMIT 1;

-- name: GetBrowserLoginLeaseForUser :one
SELECT * FROM browser_lease
WHERE id = $1 AND workspace_id = $2 AND user_id = $3 AND class = 'H';

-- name: ListExpiredDecisionBrowserLeases :many
-- Decisions that passed their expires_at while a queued or active lease still names them
-- (CHE-4742, W3-13). The worker revokes each pair; a revoked lease drops out of this list.
SELECT DISTINCT lease.workspace_id, lease.decision_id
FROM browser_lease lease
JOIN decision_request request ON request.id = lease.decision_id AND request.workspace_id = lease.workspace_id
WHERE lease.state IN ('queued', 'active')
  AND request.expires_at IS NOT NULL AND request.expires_at <= clock_timestamp()
ORDER BY lease.workspace_id, lease.decision_id
LIMIT LEAST(GREATEST(sqlc.arg('batch_size')::int, 0), 100);

-- name: IsBrowserGrantIssueClosed :one
-- A once-per-issue grant ends when its issue reaches a done or closed status (CHE-4782, W3-19).
-- A missing issue counts as closed: the grant covers nothing.
SELECT COALESCE((
    SELECT i.status IN ('done', 'cancelled') OR EXISTS (
        SELECT 1 FROM issue_status s
        WHERE s.workspace_id = i.workspace_id AND s.key = i.status AND s.category IN ('done', 'closed', 'cancelled')
    )
    FROM issue i WHERE i.id = $1 AND i.workspace_id = $2
), true)::bool AS closed;
