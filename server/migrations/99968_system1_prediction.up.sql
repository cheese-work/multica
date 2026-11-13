SET LOCAL lock_timeout = '2s';

-- One prediction per predictor version per System 1 observation (CHE-4383 B3).
-- Many predictors can guess the same observation, so it is its own table. Rows
-- are written in the ask transaction, before any answer. No foreign key:
-- observation_id is set by the insert itself (see CreateSystem1Prediction).
CREATE TABLE system1_prediction (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    observation_id uuid NOT NULL,
    scope text NOT NULL,
    -- waiver_rule | precedent | later jev. The version is part of the identity: a new version qualifies on its own.
    predictor_id text NOT NULL,
    predictor_version text NOT NULL,
    -- NULL = abstained.
    predicted_key text,
    predicted_confidence real,
    predicted_precedents integer,
    predicted_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (observation_id, predictor_id, predictor_version)
);
