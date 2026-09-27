-- CHE-704 / C01 persistence primitives. No query here advances a lifecycle,
-- admits a task, invokes a model, or applies a correction.

-- name: InsertGovernanceCase :one
INSERT INTO governance_case (
    workspace_id, subject_type, subject_id, subject_revision, rule_id,
    generation, material_fingerprint, state, authority_lineage, trigger_aliases,
    evidence_digest, rule_revision, activation_revision, config_revision,
    budget_root_id, frozen_strategy, absolute_deadline
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17
)
RETURNING *;

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
