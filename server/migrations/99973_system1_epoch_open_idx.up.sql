-- At most one open epoch per scope, predictor and version; the prediction trigger relies on it.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS system1_epoch_open
    ON system1_epoch (workspace_id, scope, predictor_id, predictor_version)
    WHERE demoted_at IS NULL;
