SET LOCAL lock_timeout = '2s';

-- The first prediction of a predictor version in a scope opens its epoch at that prediction's time.
CREATE OR REPLACE FUNCTION system1_open_epoch() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO system1_epoch (workspace_id, scope, predictor_id, predictor_version, started_at)
    VALUES (NEW.workspace_id, NEW.scope, NEW.predictor_id, NEW.predictor_version, NEW.predicted_at)
    ON CONFLICT (workspace_id, scope, predictor_id, predictor_version) WHERE demoted_at IS NULL DO NOTHING;
    RETURN NEW;
END $$;

CREATE TRIGGER system1_prediction_open_epoch AFTER INSERT ON system1_prediction
    FOR EACH ROW EXECUTE FUNCTION system1_open_epoch();

-- Predictions written before this migration (B3 waiver predictors) start their epoch at their first row.
INSERT INTO system1_epoch (workspace_id, scope, predictor_id, predictor_version, started_at)
SELECT workspace_id, scope, predictor_id, predictor_version, min(predicted_at)
FROM system1_prediction GROUP BY workspace_id, scope, predictor_id, predictor_version
ON CONFLICT DO NOTHING;
