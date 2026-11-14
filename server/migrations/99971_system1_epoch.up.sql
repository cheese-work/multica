SET LOCAL lock_timeout = '2s';

-- Qualification epoch (CHE-4383 B5): one row per (scope, predictor_id, predictor_version) period. Only observations created at or after started_at count; there is no backfill. A new predictor version has its own key, so it opens its own epoch. demoted_at closes the epoch (B7b). No foreign key: the prediction insert opens the row (see 99973a).
CREATE TABLE system1_epoch (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    scope text NOT NULL,
    predictor_id text NOT NULL,
    predictor_version text NOT NULL,
    started_at timestamptz NOT NULL,
    demoted_at timestamptz
);
