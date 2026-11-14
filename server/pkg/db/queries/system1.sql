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

-- name: CreateSystem1Prediction :exec
-- One predictor's guess for the decision observation of a request (CHE-4383 B3).
-- Runs in the ask transaction right after CreateSystem1Observation, which holds
-- the workspace FOR KEY SHARE lock; if that insert found no workspace, this finds
-- no observation and inserts nothing. A NULL key is an abstention.
INSERT INTO system1_prediction (workspace_id, observation_id, scope, predictor_id, predictor_version, predicted_key, predicted_confidence, predicted_precedents)
SELECT o.workspace_id, o.id, o.inputs->>'topic', sqlc.arg('predictor_id')::text, sqlc.arg('predictor_version')::text, sqlc.narg('predicted_key')::text, sqlc.narg('predicted_confidence')::real, sqlc.narg('predicted_precedents')::int
FROM system1_observation o
WHERE o.kind = 'decision' AND o.subject_id = sqlc.arg('subject_id') AND o.workspace_id = sqlc.arg('workspace_id');

-- name: ListSystem1WaiverPrecedentKeys :many
-- Earlier mapped waiver labels of record, newest first. `unmapped` and superseded labels are not precedents.
SELECT label::text AS label FROM system1_label
WHERE workspace_id = sqlc.arg('workspace_id') AND scope = 'test_first_waiver' AND kind = 'burn_in'
  AND label IN ('waive', 'refuse') AND superseded_at IS NULL
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

-- name: System1QualificationReport :many
-- Qualification counts per (scope, predictor, version), over the latest epoch of each (CHE-4383 B5).
-- A case is one prediction row whose observation was created at or after the epoch start and has a
-- label of record: the newest label row that is not superseded. `unmapped` and abstentions are
-- counted apart; the denominator is every non-abstaining prediction with a mapped label, so a
-- label of another protocol or `unknown` is a disagreement, never an exclusion.
WITH latest AS (
    SELECT DISTINCT ON (s.scope, s.predictor_id, s.predictor_version) s.*
    FROM system1_epoch s
    WHERE s.workspace_id = sqlc.arg('workspace_id')
      AND (sqlc.narg('scope')::text IS NULL OR s.scope = sqlc.narg('scope'))
    ORDER BY s.scope, s.predictor_id, s.predictor_version, s.started_at DESC, s.id DESC
), cases AS (
    SELECT e.id AS epoch_id, p.predicted_key, o.reversed_at, l.label
    FROM latest e
    JOIN system1_prediction p ON p.workspace_id = e.workspace_id AND p.scope = e.scope
        AND p.predictor_id = e.predictor_id AND p.predictor_version = e.predictor_version
    JOIN system1_observation o ON o.id = p.observation_id AND o.created_at >= e.started_at
    JOIN LATERAL (
        SELECT sl.label FROM system1_label sl
        WHERE sl.observation_id = o.id AND sl.kind = 'burn_in' AND sl.superseded_at IS NULL
        ORDER BY sl.created_at DESC, sl.id DESC LIMIT 1
    ) l ON true
)
SELECT e.scope::text AS scope, e.predictor_id::text AS predictor_id, e.predictor_version::text AS predictor_version,
    e.started_at::timestamptz AS started_at, (e.demoted_at IS NOT NULL)::boolean AS demoted,
    count(c.epoch_id) FILTER (WHERE c.label <> 'unmapped' AND c.predicted_key IS NOT NULL)::bigint AS denominator,
    count(c.epoch_id) FILTER (WHERE c.label <> 'unmapped' AND c.predicted_key = c.label)::bigint AS agreed,
    count(c.epoch_id) FILTER (WHERE c.label <> 'unmapped' AND c.predicted_key IS NULL)::bigint AS abstained,
    count(c.epoch_id) FILTER (WHERE c.label = 'unmapped')::bigint AS unmapped,
    count(c.epoch_id) FILTER (WHERE c.label <> 'unmapped' AND c.predicted_key IS NOT NULL AND c.reversed_at IS NOT NULL)::bigint AS reversed
FROM latest e
LEFT JOIN cases c ON c.epoch_id = e.id
GROUP BY e.id, e.scope, e.predictor_id, e.predictor_version, e.started_at, e.demoted_at
ORDER BY e.scope, e.predictor_id, e.predictor_version;
