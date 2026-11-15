-- CI token broker audit (CHE-5299, S2 of CHE-4257, rule 10). One row per
-- issuance request. There is deliberately no column for a token or a JWT.
CREATE TABLE IF NOT EXISTS ci_broker_audit (
    id                 BIGSERIAL   PRIMARY KEY,
    at                 TIMESTAMPTZ NOT NULL,
    repository_id      BIGINT      NOT NULL,
    repository         TEXT        NOT NULL,
    run_id             TEXT        NOT NULL,
    run_attempt        TEXT        NOT NULL,
    workflow_ref       TEXT        NOT NULL,
    event              TEXT        NOT NULL,
    ref                TEXT        NOT NULL,
    runner_environment TEXT        NOT NULL,
    environment        TEXT        NOT NULL,
    permissions        JSONB       NOT NULL,
    result             TEXT        NOT NULL CHECK (result IN ('issued', 'refused')),
    reason             TEXT        NOT NULL DEFAULT ''
);
