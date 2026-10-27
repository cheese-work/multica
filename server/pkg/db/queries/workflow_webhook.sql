-- name: CreateWorkflowWebhookEndpoint :one
INSERT INTO workflow_webhook_endpoint (
    workspace_id, workflow_id, name, secret_sealed, rate_per_min, created_by
) VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetWorkflowWebhookEndpoint :one
SELECT * FROM workflow_webhook_endpoint WHERE id = $1;

-- name: LockWorkflowWebhookEndpoint :one
-- Row lock that serialises admission for one endpoint (spec: Admission transaction).
SELECT * FROM workflow_webhook_endpoint WHERE id = $1 FOR UPDATE;

-- name: ListWorkflowWebhookEndpoints :many
SELECT * FROM workflow_webhook_endpoint
WHERE workspace_id = $1 AND workflow_id = $2
ORDER BY created_at, id;

-- name: RotateWorkflowWebhookEndpointSecret :one
UPDATE workflow_webhook_endpoint
SET previous_secret_sealed = secret_sealed,
    previous_expires_at = @previous_expires_at,
    secret_sealed = @secret_sealed
WHERE id = @id AND disabled_at IS NULL
RETURNING *;

-- name: DisableWorkflowWebhookEndpoint :one
UPDATE workflow_webhook_endpoint
SET disabled_at = now(), previous_secret_sealed = NULL, previous_expires_at = NULL
WHERE id = $1 AND disabled_at IS NULL
RETURNING *;

-- name: CountWorkflowWebhookNoncesSince :one
SELECT count(*)::bigint AS n FROM workflow_webhook_nonce
WHERE endpoint_id = $1 AND seen_at > $2;

-- name: InsertWorkflowWebhookNonce :execrows
-- Zero rows inserted means the nonce was already used: a replay.
INSERT INTO workflow_webhook_nonce (endpoint_id, nonce) VALUES ($1, $2)
ON CONFLICT (endpoint_id, nonce) DO NOTHING;

-- name: DeleteExpiredWorkflowWebhookNonces :execrows
DELETE FROM workflow_webhook_nonce WHERE seen_at < $1;

-- name: InsertWorkflowWebhookEvent :execrows
-- A retry with a new nonce but the same ref is a no-op (zero rows).
INSERT INTO workflow_webhook_event (
    endpoint_id, workspace_id, issue_id, name, result, ref, body_sha256, body_bytes
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (endpoint_id, issue_id, ref) WHERE ref IS NOT NULL DO NOTHING;

-- name: GetLatestWorkflowWebhookSuccess :one
-- Guard lookup: keyed by endpoint and issue, never by name alone.
SELECT * FROM workflow_webhook_event
WHERE endpoint_id = $1 AND issue_id = $2 AND result = 'success' AND received_at >= $3
ORDER BY received_at DESC
LIMIT 1;

-- name: GetWorkflowWebhookEndpointByName :one
SELECT * FROM workflow_webhook_endpoint
WHERE workflow_id = $1 AND name = $2;

-- name: WakeWorkflowEdges :exec
-- Prompts the engine to look now. Only a prompt: the engine reads committed
-- events whether or not this ran.
UPDATE issue_wakeup SET next_fire_at = now(), updated_at = clock_timestamp()
WHERE issue_id = $1 AND system_rule = 'workflow_edges';
