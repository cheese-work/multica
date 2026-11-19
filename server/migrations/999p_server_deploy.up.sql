-- One row per server deploy (CHE-5709, plan D14): written at server start when
-- the running commit differs from the newest row. Feeds the "server update
-- windows" of the machine Planned tab. No foreign keys.
CREATE TABLE IF NOT EXISTS server_deploy (
    id BIGSERIAL PRIMARY KEY,
    "commit" TEXT NOT NULL,
    version TEXT NOT NULL DEFAULT '',
    started_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS server_deploy_started_at_idx ON server_deploy (started_at DESC);
