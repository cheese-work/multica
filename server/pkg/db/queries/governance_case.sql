-- CHE-704 / C01 persistence and C03 lifecycle primitives. These queries never
-- invoke a provider or apply a correction.

-- The transaction-backed lifecycle primitives take this lock before reading
-- or allocating a generation. It spans source revisions for one subject/rule.
-- A separate statement is required so a waiter gets a post-lock snapshot.
-- name: LockGovernanceCaseIdentity :exec
SELECT pg_advisory_xact_lock(hashtextextended(
    sqlc.arg(workspace_id)::uuid::text || ':' || sqlc.arg(subject_type) || ':' ||
    sqlc.arg(subject_id)::uuid::text || ':' || sqlc.arg(rule_id)::uuid::text || ':governance_case',
    0
));

-- name: LockGovernanceCaseForUpdate :one
SELECT * FROM governance_case
WHERE workspace_id = $1 AND id = $2
FOR UPDATE;

-- name: FindGovernanceCaseByMaterialFingerprint :one
SELECT * FROM governance_case
WHERE workspace_id = $1
  AND subject_type = $2
  AND subject_id = $3
  AND subject_revision = $4
  AND rule_id = $5
  AND material_fingerprint = $6;

-- name: InsertNextGovernanceCase :one
INSERT INTO governance_case (
    workspace_id, subject_type, subject_id, subject_revision, rule_id,
    generation, material_fingerprint, state, authority_lineage, trigger_aliases,
    evidence_digest, rule_revision, activation_revision, config_revision,
    predecessor_case_id, budget_root_id, frozen_strategy, absolute_deadline,
    evidence_epoch, refresh_count
)
SELECT
    $1, $2, $3, $4, $5,
    COALESCE((
        SELECT max(gc.generation) + 1
        FROM governance_case gc
        WHERE gc.workspace_id = $1
          AND gc.subject_type = $2
          AND gc.subject_id = $3
          AND gc.rule_id = $5
    ), 0),
    $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19
RETURNING *;

-- name: FindGovernanceCaseTransitionByCause :one
SELECT * FROM governance_case_transition
WHERE workspace_id = $1 AND case_id = $2 AND cause_event_key = $3;

-- name: UpdateGovernanceCaseTransitionCAS :one
UPDATE governance_case
SET state = sqlc.arg(next_state)::text,
    state_revision = state_revision + 1,
    lease_token = NULL,
    lease_expires_at = NULL,
    reason = sqlc.arg(reason)::text,
    updated_at = sqlc.arg(updated_at)::timestamptz
WHERE workspace_id = sqlc.arg(workspace_id)::uuid
  AND id = sqlc.arg(id)::uuid
  AND state = sqlc.arg(expected_state)::text
  AND state_revision = sqlc.arg(expected_state_revision)::bigint
RETURNING *;

-- name: UpdateGovernanceCaseLease :one
UPDATE governance_case
SET lease_token = $4, lease_expires_at = $5, updated_at = $6
WHERE workspace_id = $1 AND id = $2 AND state_revision = $3
RETURNING *;

-- name: ReleaseGovernanceCaseLease :execrows
UPDATE governance_case
SET lease_token = NULL, lease_expires_at = NULL, updated_at = $4
WHERE workspace_id = $1 AND id = $2 AND lease_token = $3;

-- name: InsertGovernanceCaseTransition :one
INSERT INTO governance_case_transition (
    workspace_id, case_id, resulting_state_revision, expected_state_revision,
    from_state, to_state, cause_event_key, actor_type, actor_id, sanitized_reason
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
)
RETURNING *;

-- name: InsertGovernanceAttempt :one
INSERT INTO governance_attempt (
    workspace_id, case_id, ordinal, kind, candidate_id, task_id, obligation_id,
    input_digest, attempt_fence, deadline_at, confidence, result, usage
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13
)
RETURNING *;

-- name: LockGovernanceAttemptForUpdate :one
SELECT * FROM governance_attempt
WHERE workspace_id = $1 AND case_id = $2 AND id = $3
FOR UPDATE;

-- name: InsertGovernanceEvaluation :one
INSERT INTO governance_evaluation (
    workspace_id, case_id, attempt_id, trigger_identity, subject_revision_vector,
    snapshot, snapshot_digest, snapshot_schema_version, required_complete,
    candidate_map, citation_map, estimated_tokens, applicable_rule_digests,
    question_criteria_hash, requested_model, returned_model, answers
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17
)
RETURNING *;

-- name: InsertGovernanceEvaluationSource :one
INSERT INTO governance_evaluation_source (
    workspace_id, evaluation_id, object_type, object_id, object_revision,
    object_digest, copied_context
) VALUES (
    $1, $2, $3, $4, $5, $6, $7
)
RETURNING *;

-- name: ListGovernanceCasesPage :many
-- Bounded workspace-leading keyset page for future case listing. The first
-- page passes has_cursor=false; later pages pass the last created_at/id pair.
SELECT * FROM governance_case
WHERE workspace_id = $1
  AND (
      @has_cursor::boolean = FALSE
      OR (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
  )
ORDER BY created_at DESC, id DESC
LIMIT $2;
