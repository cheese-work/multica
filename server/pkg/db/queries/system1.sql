-- name: CreateSystem1Observation :one
INSERT INTO system1_observation (workspace_id, kind, subject_id, class, decision_source, inputs, options, recommended_index, agent_confidence, predicted_key, predicted_confidence, predicted_precedents, predicted_at, choice_key, outcome, answered_at)
VALUES (sqlc.arg('workspace_id'), sqlc.arg('kind'), sqlc.arg('subject_id'), sqlc.arg('class'), sqlc.narg('decision_source'), sqlc.arg('inputs')::jsonb, sqlc.arg('options')::jsonb, sqlc.narg('recommended_index'), sqlc.narg('agent_confidence'), sqlc.narg('predicted_key'), sqlc.narg('predicted_confidence'), sqlc.narg('predicted_precedents'), clock_timestamp(), sqlc.narg('choice_key'), sqlc.narg('outcome'), CASE WHEN sqlc.narg('outcome')::text = 'applied' THEN clock_timestamp() END)
RETURNING *;

-- name: ListSystem1PrecedentKeys :many
SELECT choice_key::text AS choice_key FROM system1_observation
WHERE workspace_id = sqlc.arg('workspace_id') AND kind = sqlc.arg('kind') AND class = sqlc.arg('class')
  AND choice_key IS NOT NULL AND reversed_at IS NULL
ORDER BY created_at DESC
LIMIT 200;

-- name: MarkSystem1RoutingOverridden :exec
-- A person changed the assignee a workflow handoff had just chosen.
UPDATE system1_observation SET reversed_at = clock_timestamp()
WHERE id = (
    SELECT latest.id FROM system1_observation latest
    WHERE latest.kind = 'routing' AND latest.subject_id = sqlc.arg('issue_id') AND latest.reversed_at IS NULL
    ORDER BY latest.created_at DESC LIMIT 1
) AND created_at > clock_timestamp() - interval '1 hour';

-- name: System1ReportByClass :many
-- Precision and coverage of the shadow predictions that preceded an answer.
-- other_* is the audit slice: answers where the human did not take the recommendation (another option, or free text).
SELECT class::text AS class,
    count(*) FILTER (WHERE choice_key IS NOT NULL)::bigint AS decided,
    count(*) FILTER (WHERE choice_key IS NOT NULL AND coalesce(predicted_key, '') <> '')::bigint AS predicted,
    count(*) FILTER (WHERE choice_key IS NOT NULL AND predicted_key = choice_key)::bigint AS agreed,
    count(*) FILTER (WHERE choice_key IS NOT NULL AND choice_key IN ('other', 'free_text'))::bigint AS other_decided,
    count(*) FILTER (WHERE choice_key IS NOT NULL AND choice_key IN ('other', 'free_text') AND coalesce(predicted_key, '') <> '')::bigint AS other_predicted,
    count(*) FILTER (WHERE choice_key IS NOT NULL AND choice_key IN ('other', 'free_text') AND predicted_key = choice_key)::bigint AS other_agreed,
    count(*) FILTER (WHERE reversed_at IS NOT NULL)::bigint AS reversed,
    coalesce(percentile_cont(0.5) WITHIN GROUP (ORDER BY time_to_answer_ms), 0)::float8 AS median_answer_ms
FROM system1_observation
WHERE workspace_id = sqlc.arg('workspace_id') AND kind = sqlc.arg('kind')
GROUP BY class
ORDER BY class;
