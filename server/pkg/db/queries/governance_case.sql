-- CHE-704 / C01 persistence primitives. No query here advances a lifecycle,
-- admits a task, invokes a model, or applies a correction.

-- The transaction-backed create-or-resolve primitive takes this lock before
-- reading or allocating a generation. A separate lock statement is required:
-- a single CTE would retain a pre-lock snapshot after waiting for a peer.
-- name: LockGovernanceCaseIdentity :exec
SELECT pg_advisory_xact_lock(hashtextextended(
    sqlc.arg(workspace_id)::uuid::text || ':' || sqlc.arg(subject_type) || ':' ||
    sqlc.arg(subject_id)::uuid::text || ':' || sqlc.arg(subject_revision)::bigint::text || ':' ||
    sqlc.arg(rule_id)::uuid::text || ':governance_case',
    0
));

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
    budget_root_id, frozen_strategy, absolute_deadline
)
SELECT
    $1, $2, $3, $4, $5,
    COALESCE((
        SELECT max(gc.generation) + 1
        FROM governance_case gc
        WHERE gc.workspace_id = $1
          AND gc.subject_type = $2
          AND gc.subject_id = $3
          AND gc.subject_revision = $4
          AND gc.rule_id = $5
    ), 0),
    $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16
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
WITH locked_issue AS MATERIALIZED (
    SELECT issue.id
    FROM issue
    WHERE sqlc.arg('object_type')::text = 'issue'
      AND issue.workspace_id = sqlc.arg('workspace_id')::uuid
      AND issue.id::text = sqlc.arg('object_id')::text
    FOR KEY SHARE OF issue
), locked_comment_issue AS MATERIALIZED (
    SELECT issue.id
    FROM issue
    JOIN comment
      ON comment.issue_id = issue.id
     AND comment.workspace_id = issue.workspace_id
    WHERE sqlc.arg('object_type')::text = 'comment'
      AND issue.workspace_id = sqlc.arg('workspace_id')::uuid
      AND comment.id::text = sqlc.arg('object_id')::text
      AND comment.deleted_at IS NULL
    FOR KEY SHARE OF issue
), locked_comment AS MATERIALIZED (
    SELECT comment.id
    FROM comment
    JOIN locked_comment_issue ON locked_comment_issue.id = comment.issue_id
    WHERE comment.workspace_id = sqlc.arg('workspace_id')::uuid
      AND comment.id::text = sqlc.arg('object_id')::text
      AND comment.deleted_at IS NULL
    FOR UPDATE OF comment
), live_source AS MATERIALIZED (
    SELECT id FROM locked_issue
    UNION ALL
    SELECT id FROM locked_comment
)
INSERT INTO governance_evaluation_source (
    workspace_id, evaluation_id, object_type, object_id, object_revision,
    object_digest, copied_context
)
SELECT sqlc.arg('workspace_id')::uuid, sqlc.arg('evaluation_id')::uuid,
       sqlc.arg('object_type')::text, live_source.id::text,
       sqlc.arg('object_revision')::text, sqlc.arg('object_digest')::text,
       sqlc.arg('copied_context')::jsonb
FROM live_source
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

-- name: ListGovernanceCaseAuditPage :many
SELECT id, workspace_id, subject_type, subject_id, subject_revision, rule_id,
       generation, state, state_revision, rule_revision, activation_revision,
       config_revision, evidence_epoch, refresh_count, absolute_deadline,
       created_at, updated_at
FROM governance_case
WHERE workspace_id = sqlc.arg('workspace_id')::uuid
  AND (
      sqlc.arg('has_cursor')::boolean = FALSE
      OR (created_at, id) < (
          sqlc.arg('cursor_created_at')::timestamptz,
          sqlc.arg('cursor_id')::uuid
      )
  )
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('page_size')::integer;

-- name: GetGovernanceCaseAudit :one
SELECT id, workspace_id, subject_type, subject_id, subject_revision, rule_id,
       generation, state, state_revision, rule_revision, activation_revision,
       config_revision, evidence_epoch, refresh_count, absolute_deadline,
       created_at, updated_at
FROM governance_case
WHERE workspace_id = sqlc.arg('workspace_id')::uuid
  AND id = sqlc.arg('case_id')::uuid;

-- name: ListGovernanceCaseAuditTransitions :many
SELECT id, resulting_state_revision, expected_state_revision, from_state,
       to_state, actor_type, created_at
FROM governance_case_transition
WHERE workspace_id = sqlc.arg('workspace_id')::uuid
  AND case_id = sqlc.arg('case_id')::uuid
  AND (
      sqlc.arg('has_cursor')::boolean = FALSE
      OR (created_at, id) < (
          sqlc.arg('cursor_created_at')::timestamptz,
          sqlc.arg('cursor_id')::uuid
      )
  )
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('page_size')::integer;

-- name: ListGovernanceCaseAuditAttempts :many
SELECT id, ordinal, kind, obligation_id, deadline_at, terminal_at, created_at
FROM governance_attempt
WHERE workspace_id = sqlc.arg('workspace_id')::uuid
  AND case_id = sqlc.arg('case_id')::uuid
  AND (
      sqlc.arg('has_cursor')::boolean = FALSE
      OR (created_at, id) < (
          sqlc.arg('cursor_created_at')::timestamptz,
          sqlc.arg('cursor_id')::uuid
      )
  )
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('page_size')::integer;

-- name: ListGovernanceCaseAuditEvaluations :many
SELECT id, snapshot_schema_version, required_complete, estimated_tokens,
       captured_at, redacted_at
FROM governance_evaluation
WHERE workspace_id = sqlc.arg('workspace_id')::uuid
  AND case_id = sqlc.arg('case_id')::uuid
  AND (
      sqlc.arg('has_cursor')::boolean = FALSE
      OR (captured_at, id) < (
          sqlc.arg('cursor_captured_at')::timestamptz,
          sqlc.arg('cursor_id')::uuid
      )
  )
ORDER BY captured_at DESC, id DESC
LIMIT sqlc.arg('page_size')::integer;

-- name: RedactGovernanceEvidenceForSource :exec
WITH affected AS MATERIALIZED (
    SELECT DISTINCT source_index.evaluation_id
    FROM governance_evaluation_source AS source_index
    WHERE source_index.workspace_id = sqlc.arg('workspace_id')::uuid
      AND source_index.object_type = sqlc.arg('object_type')
      AND source_index.object_id = sqlc.arg('object_id')
), redacted_sources AS (
    UPDATE governance_evaluation_source source
    SET copied_context = '{}'::jsonb,
        object_digest = '',
        redacted_at = COALESCE(source.redacted_at, now())
    FROM affected
    WHERE source.workspace_id = sqlc.arg('workspace_id')::uuid
      AND source.evaluation_id = affected.evaluation_id
      AND source.redacted_at IS NULL
    RETURNING source.evaluation_id
), redacted_evaluations AS (
    UPDATE governance_evaluation evaluation
    SET snapshot = '{}'::jsonb,
        snapshot_digest = '',
        required_complete = false,
        candidate_map = '[]'::jsonb,
        citation_map = '[]'::jsonb,
        estimated_tokens = 0,
        requested_model = '',
        returned_model = '',
        answers = '[]'::jsonb,
        redacted_at = COALESCE(evaluation.redacted_at, now())
    FROM affected
    WHERE evaluation.workspace_id = sqlc.arg('workspace_id')::uuid
      AND evaluation.id = affected.evaluation_id
      AND evaluation.redacted_at IS NULL
    RETURNING evaluation.attempt_id
), redacted_cases AS (
    UPDATE governance_case case_record
    SET trigger_aliases = '[]'::jsonb,
        evidence_digest = '',
        reason = ''
    FROM governance_evaluation evaluation, affected
    WHERE evaluation.workspace_id = sqlc.arg('workspace_id')::uuid
      AND evaluation.id = affected.evaluation_id
      AND case_record.workspace_id = sqlc.arg('workspace_id')::uuid
      AND case_record.id = evaluation.case_id
    RETURNING case_record.id
)
UPDATE governance_attempt attempt
SET input_digest = '',
    terminal_reason = CASE WHEN attempt.terminal_at IS NOT NULL THEN 'redacted' ELSE '' END,
    confidence = '{}'::jsonb,
    result = '{}'::jsonb,
    usage = '{}'::jsonb,
    redacted_at = COALESCE(attempt.redacted_at, now())
WHERE attempt.id IN (
    SELECT attempt_id FROM redacted_evaluations WHERE attempt_id IS NOT NULL
)
  AND attempt.workspace_id = sqlc.arg('workspace_id')::uuid;

-- name: ListGovernanceCaseRetentionWorkspaces :many
SELECT DISTINCT workspace_id
FROM governance_case
ORDER BY workspace_id;

-- name: RedactExpiredGovernanceEvidenceForWorkspace :exec
WITH affected AS MATERIALIZED (
    SELECT evaluation.id, evaluation.case_id
    FROM governance_evaluation AS evaluation
    WHERE evaluation.workspace_id = sqlc.arg('workspace_id')::uuid
      AND evaluation.captured_at < now() - interval '30 days'
      AND evaluation.redacted_at IS NULL
), redacted_sources AS (
    UPDATE governance_evaluation_source AS source
    SET copied_context = '{}'::jsonb,
        object_digest = '',
        redacted_at = COALESCE(source.redacted_at, now())
    FROM affected
    WHERE source.workspace_id = sqlc.arg('workspace_id')::uuid
      AND source.evaluation_id = affected.id
      AND source.redacted_at IS NULL
    RETURNING source.evaluation_id
), expired_cases AS MATERIALIZED (
    SELECT case_record.id
    FROM governance_case case_record
    WHERE case_record.workspace_id = sqlc.arg('workspace_id')::uuid
      AND case_record.created_at < now() - interval '30 days'
    UNION
    SELECT affected.case_id
    FROM affected
), redacted_cases AS (
    UPDATE governance_case case_record
    SET trigger_aliases = '[]'::jsonb,
        evidence_digest = '',
        reason = '',
        frozen_strategy = '[]'::jsonb
    FROM expired_cases
    WHERE case_record.workspace_id = sqlc.arg('workspace_id')::uuid
      AND case_record.id = expired_cases.id
    RETURNING case_record.id
), redacted_transitions AS (
    UPDATE governance_case_transition transition
    SET sanitized_reason = ''
    WHERE transition.workspace_id = sqlc.arg('workspace_id')::uuid
      AND transition.created_at < now() - interval '30 days'
      AND transition.sanitized_reason <> ''
    RETURNING transition.id
)
UPDATE governance_evaluation AS evaluation
SET snapshot = '{}'::jsonb,
    snapshot_digest = '',
    required_complete = false,
    candidate_map = '[]'::jsonb,
    citation_map = '[]'::jsonb,
    estimated_tokens = 0,
    requested_model = '',
    returned_model = '',
    answers = '[]'::jsonb,
    redacted_at = COALESCE(evaluation.redacted_at, now())
WHERE evaluation.workspace_id = sqlc.arg('workspace_id')::uuid
  AND evaluation.id IN (SELECT id FROM affected)
  AND evaluation.redacted_at IS NULL;

-- name: RedactExpiredGovernanceAttemptResultsForWorkspace :exec
UPDATE governance_attempt AS attempt
SET input_digest = '',
    terminal_reason = CASE WHEN attempt.terminal_at IS NOT NULL THEN 'redacted' ELSE '' END,
    confidence = '{}'::jsonb,
    result = '{}'::jsonb,
    usage = '{}'::jsonb,
    redacted_at = COALESCE(attempt.redacted_at, now())
WHERE attempt.workspace_id = sqlc.arg('workspace_id')::uuid
  AND (
      attempt.created_at < now() - interval '30 days'
      OR EXISTS (
          SELECT 1
          FROM governance_evaluation AS evaluation
          WHERE evaluation.workspace_id = sqlc.arg('workspace_id')::uuid
            AND evaluation.attempt_id = attempt.id
            AND evaluation.captured_at < now() - interval '30 days'
      )
  )
  AND attempt.redacted_at IS NULL;

-- name: DeleteExpiredGovernanceEvaluationSourcesForWorkspace :execrows
DELETE FROM governance_evaluation_source AS source
USING governance_evaluation AS evaluation, governance_case AS governance_case
WHERE source.workspace_id = sqlc.arg('workspace_id')::uuid
  AND source.evaluation_id = evaluation.id
  AND evaluation.workspace_id = sqlc.arg('workspace_id')::uuid
  AND evaluation.case_id = governance_case.id
  AND governance_case.workspace_id = sqlc.arg('workspace_id')::uuid
  AND governance_case.state IN ('resolved', 'dismissed', 'invalidated', 'failed', 'parked')
  AND evaluation.captured_at < now() - interval '90 days';

-- name: DeleteExpiredGovernanceEvaluationsForWorkspace :execrows
DELETE FROM governance_evaluation AS evaluation
USING governance_case AS governance_case
WHERE evaluation.workspace_id = sqlc.arg('workspace_id')::uuid
  AND evaluation.case_id = governance_case.id
  AND governance_case.workspace_id = sqlc.arg('workspace_id')::uuid
  AND governance_case.state IN ('resolved', 'dismissed', 'invalidated', 'failed', 'parked')
  AND evaluation.captured_at < now() - interval '90 days';

-- name: DeleteExpiredGovernanceTransitionsForWorkspace :execrows
DELETE FROM governance_case_transition AS transition
USING governance_case AS case_record
WHERE transition.workspace_id = sqlc.arg('workspace_id')::uuid
  AND transition.case_id = case_record.id
  AND case_record.workspace_id = sqlc.arg('workspace_id')::uuid
  AND case_record.state IN ('resolved', 'dismissed', 'invalidated', 'failed', 'parked')
  AND transition.created_at < now() - interval '90 days';

-- name: DeleteExpiredGovernanceAttemptsForWorkspace :execrows
DELETE FROM governance_attempt AS attempt
USING governance_case AS case_record
WHERE attempt.workspace_id = sqlc.arg('workspace_id')::uuid
  AND attempt.case_id = case_record.id
  AND case_record.workspace_id = sqlc.arg('workspace_id')::uuid
  AND case_record.state IN ('resolved', 'dismissed', 'invalidated', 'failed', 'parked')
  AND attempt.created_at < now() - interval '90 days'
  AND attempt.terminal_at IS NOT NULL
  AND attempt.obligation_id IS NULL;

-- name: PruneExpiredGovernanceObligationAttemptMetadata :execrows
UPDATE governance_attempt AS attempt
SET candidate_id = NULL,
    task_id = NULL,
    input_digest = '',
    deadline_at = NULL,
    terminal_reason = 'expired',
    confidence = '{}'::jsonb,
    result = '{}'::jsonb,
    usage = '{}'::jsonb
FROM governance_case AS case_record
WHERE attempt.workspace_id = sqlc.arg('workspace_id')::uuid
  AND attempt.case_id = case_record.id
  AND case_record.workspace_id = sqlc.arg('workspace_id')::uuid
  AND case_record.state IN ('resolved', 'dismissed', 'invalidated', 'failed', 'parked')
  AND attempt.created_at < now() - interval '90 days'
  AND attempt.terminal_at IS NOT NULL
  AND attempt.obligation_id IS NOT NULL;

-- name: RedactGovernanceEvidenceForIssue :exec
WITH affected AS MATERIALIZED (
    SELECT DISTINCT source_index.evaluation_id
    FROM governance_evaluation_source AS source_index
    WHERE source_index.workspace_id = sqlc.arg('workspace_id')::uuid
      AND (
          (source_index.object_type = 'issue' AND source_index.object_id = sqlc.arg('issue_id')::uuid::text)
          OR (
              source_index.object_type = 'comment'
              AND source_index.object_id IN (
                  SELECT comment.id::text
                  FROM comment
                  WHERE comment.workspace_id = sqlc.arg('workspace_id')::uuid
                    AND comment.issue_id = sqlc.arg('issue_id')::uuid
              )
          )
      )
), redacted_sources AS (
    UPDATE governance_evaluation_source source
    SET copied_context = '{}'::jsonb,
        object_digest = '',
        redacted_at = COALESCE(source.redacted_at, now())
    FROM affected
    WHERE source.workspace_id = sqlc.arg('workspace_id')::uuid
      AND source.evaluation_id = affected.evaluation_id
      AND source.redacted_at IS NULL
    RETURNING source.evaluation_id
), redacted_evaluations AS (
    UPDATE governance_evaluation evaluation
    SET snapshot = '{}'::jsonb,
        snapshot_digest = '',
        required_complete = false,
        candidate_map = '[]'::jsonb,
        citation_map = '[]'::jsonb,
        estimated_tokens = 0,
        requested_model = '',
        returned_model = '',
        answers = '[]'::jsonb,
        redacted_at = COALESCE(evaluation.redacted_at, now())
    FROM affected
    WHERE evaluation.workspace_id = sqlc.arg('workspace_id')::uuid
      AND evaluation.id = affected.evaluation_id
      AND evaluation.redacted_at IS NULL
    RETURNING evaluation.attempt_id
), redacted_cases AS (
    UPDATE governance_case case_record
    SET trigger_aliases = '[]'::jsonb,
        evidence_digest = '',
        reason = ''
    FROM governance_evaluation evaluation, affected
    WHERE evaluation.workspace_id = sqlc.arg('workspace_id')::uuid
      AND evaluation.id = affected.evaluation_id
      AND case_record.workspace_id = sqlc.arg('workspace_id')::uuid
      AND case_record.id = evaluation.case_id
    RETURNING case_record.id
)
UPDATE governance_attempt attempt
SET input_digest = '',
    terminal_reason = CASE WHEN attempt.terminal_at IS NOT NULL THEN 'redacted' ELSE '' END,
    confidence = '{}'::jsonb,
    result = '{}'::jsonb,
    usage = '{}'::jsonb,
    redacted_at = COALESCE(attempt.redacted_at, now())
WHERE attempt.id IN (
    SELECT attempt_id FROM redacted_evaluations WHERE attempt_id IS NOT NULL
)
  AND attempt.workspace_id = sqlc.arg('workspace_id')::uuid;
