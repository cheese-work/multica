-- name: CreateSystem1Observation :exec
-- Inserts only while the workspace exists. FOR KEY SHARE conflicts with the FOR
-- UPDATE that workspace deletion takes first, so a capture that overlaps a
-- deletion waits, then finds no workspace and inserts nothing: the table has no
-- FK to catch the orphan.
INSERT INTO system1_observation (workspace_id, kind, subject_id, class, decision_source, inputs, option_count, recommended_index, agent_confidence, predicted_key, predicted_confidence, predicted_precedents, predicted_at, choice_key, outcome, answered_at, event_id)
SELECT workspace.id, sqlc.arg('kind')::text, sqlc.arg('subject_id')::uuid, sqlc.arg('class')::text, sqlc.narg('decision_source')::text, sqlc.arg('inputs')::jsonb, sqlc.arg('option_count')::int, sqlc.narg('recommended_index')::int, sqlc.narg('agent_confidence')::real, sqlc.narg('predicted_key')::text, sqlc.narg('predicted_confidence')::real, sqlc.narg('predicted_precedents')::int, clock_timestamp(), sqlc.narg('choice_key')::text, sqlc.narg('outcome')::text, CASE WHEN sqlc.narg('outcome')::text = 'applied' THEN clock_timestamp() END, sqlc.narg('event_id')::uuid
FROM workspace WHERE workspace.id = sqlc.arg('workspace_id')
FOR KEY SHARE OF workspace;

-- name: ListSystem1PrecedentKeys :many
SELECT choice_key::text AS choice_key FROM system1_observation
WHERE workspace_id = sqlc.arg('workspace_id') AND kind = sqlc.arg('kind') AND class = sqlc.arg('class')
  AND choice_key IS NOT NULL AND reversed_at IS NULL
ORDER BY created_at DESC
LIMIT 200;

-- name: ListRecentIssueStatusChanges :many
-- The last few statuses an issue moved to, newest first (route label input).
SELECT (details->>'to')::text AS to_status FROM activity_log
WHERE issue_id = sqlc.arg('issue_id') AND action = 'status_changed' AND details->>'to' IS NOT NULL
ORDER BY created_at DESC, id DESC
LIMIT 3;

-- name: MarkSystem1RoutingOverridden :exec
-- A person changed the assignee a workflow handoff had just chosen.
UPDATE system1_observation SET reversed_at = clock_timestamp()
WHERE id = (
    SELECT latest.id FROM system1_observation latest
    WHERE latest.kind = 'routing' AND latest.subject_id = sqlc.arg('issue_id') AND latest.reversed_at IS NULL
    ORDER BY latest.created_at DESC LIMIT 1
) AND created_at > clock_timestamp() - interval '1 hour';

-- name: MarkSystem1RouteOverridden :exec
-- A person changed the status or owner of an issue whose latest route label
-- an agent had just written. Runs in the person's write, under the issue lock.
UPDATE system1_observation SET reversed_at = clock_timestamp()
WHERE id = (
    SELECT latest.id FROM system1_observation latest
    WHERE latest.kind = 'route' AND latest.subject_id = sqlc.arg('issue_id') AND latest.reversed_at IS NULL
    ORDER BY latest.created_at DESC, latest.id DESC LIMIT 1
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
  AND (sqlc.narg('since')::timestamptz IS NULL OR created_at >= sqlc.narg('since'))
  AND (sqlc.narg('until')::timestamptz IS NULL OR created_at < sqlc.narg('until'))
GROUP BY class
ORDER BY class;

-- name: System1RouteWindowReport :one
-- CHE-4550: one event population for the route block. eligible_exits are the
-- agent status_changed rows marked routing_exit in [since, until); matched ones
-- have an agent route row on the same event_id (any time). via_route_share counts
-- matched events whose earliest agent route row has via = route. The label counts
-- are over agent route rows created in the window and sit outside coverage.
-- Duplicates are numbered over all time, so a twin of an earlier label counts (spec 23).
WITH exits AS (
    SELECT a.id FROM activity_log a
    WHERE a.workspace_id = sqlc.arg('workspace_id') AND a.action = 'status_changed' AND a.actor_type = 'agent'
      AND a.details @> '{"routing_exit": true}'
      AND a.created_at >= sqlc.arg('since') AND a.created_at < sqlc.arg('until')
), first_label AS (
    SELECT DISTINCT ON (event_id) event_id, inputs->>'via' AS via
    FROM system1_observation
    WHERE workspace_id = sqlc.arg('workspace_id') AND kind = 'route' AND decision_source = 'agent'
      AND event_id IN (SELECT id FROM exits)
    ORDER BY event_id, created_at, id
), numbered AS (
    SELECT event_id, reversed_at, inputs, created_at,
        row_number() OVER (PARTITION BY event_id ORDER BY created_at, id) AS rn
    FROM system1_observation
    WHERE workspace_id = sqlc.arg('workspace_id') AND kind = 'route' AND decision_source = 'agent'
), labels AS (
    SELECT * FROM numbered WHERE created_at >= sqlc.arg('since') AND created_at < sqlc.arg('until')
)
SELECT
    (SELECT count(*) FROM exits)::bigint AS eligible_exits,
    (SELECT count(*) FROM first_label)::bigint AS matched_labels,
    (SELECT count(*) FROM first_label WHERE via = 'route')::bigint AS via_route_labels,
    (SELECT count(*) FROM labels WHERE event_id IS NOT NULL AND rn > 1)::bigint AS duplicate_labels,
    (SELECT count(*) FROM labels)::bigint AS labels_total,
    (SELECT count(*) FROM labels WHERE event_id IS NULL)::bigint AS owner_only_labels,
    (SELECT count(*) FROM labels WHERE event_id IS NOT NULL AND inputs->>'routing_exit' = 'false')::bigint AS non_routing_labels,
    (SELECT count(*) FROM labels WHERE reversed_at IS NOT NULL)::bigint AS overridden;

-- name: GetLatestSystem1RouteObservation :one
-- The route row a just-committed route call wrote (the issue's newest one).
SELECT id FROM system1_observation
WHERE kind = 'route' AND subject_id = sqlc.arg('issue_id')
ORDER BY created_at DESC, id DESC LIMIT 1;
